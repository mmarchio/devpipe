package service

import (
	"context"
	"os"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"

	"overseer/internal/persistence"
	"overseer/internal/project"
)

// Readiness failure codes (WBS 3.5). Each failing requirement yields one
// stable, machine-readable code so clients can react to specific gaps.
const (
	FailureWorkingDirectoryNotConfigured = "working_directory_not_configured"
	FailureWorkingDirectoryMissing       = "working_directory_missing"
	FailureWorkingDirectoryNotADirectory = "working_directory_not_a_directory"
	FailureWorkerProviderMissing         = "worker_provider_missing"
	FailureWorkerModelMissing            = "worker_model_missing"
	FailureCompressionProviderMissing    = "compression_provider_missing"
	FailureCompressionModelMissing       = "compression_model_missing"
	FailureContextConfigurationInvalid   = "context_configuration_invalid"
	FailurePhaseMissing                  = "phase_missing"
	FailurePhaseWBSMissing               = "phase_wbs_missing"
)

// ReadinessReport is the structured validation result (WBS 3.5).
// Failures lists one code per unmet requirement; Ready is true exactly
// when no requirement failed.
type ReadinessReport struct {
	Ready    bool
	Failures []string
}

// ReadinessService validates that a project has enough configuration to
// eventually execute (WBS 3.5) and performs the readiness transition
// (DRAFT → READY) when it does.
type ReadinessService struct {
	projects  persistence.ProjectRepository
	settings  persistence.SettingsRepository
	phases    persistence.PhaseRepository
	documents persistence.WBSDocumentRepository
	// lifecycle records the DRAFT → READY audit event atomically with
	// the status write (WBS 3.7); nil in unit scope, where the
	// transition degrades to a direct status update.
	lifecycle *LifecycleService
}

// wbsDocumentAdapter lets the PostgreSQL WBS-document repository (which
// returns its own record type) be used behind the domain interface.
type wbsDocumentAdapter struct{ repo *dbrepo.DocumentRepository }

func (a wbsDocumentAdapter) GetByPhase(ctx context.Context, phaseID string) (*persistence.WBSDocumentRecord, error) {
	doc, err := a.repo.GetByPhase(ctx, phaseID)
	if err != nil {
		return nil, err
	}
	return &persistence.WBSDocumentRecord{ID: doc.ID, PhaseID: doc.PhaseID, Content: doc.Content, Version: doc.Version}, nil
}

func (a wbsDocumentAdapter) Create(ctx context.Context, phaseID, content string) (*persistence.WBSDocumentRecord, error) {
	doc, err := a.repo.Create(ctx, phaseID, "WBS", content)
	if err != nil {
		return nil, err
	}
	return &persistence.WBSDocumentRecord{ID: doc.ID, PhaseID: doc.PhaseID, Content: doc.Content, Version: doc.Version}, nil
}

func (a wbsDocumentAdapter) UpdateContent(ctx context.Context, phaseID, content string) error {
	return a.repo.UpdateContent(ctx, phaseID, content)
}

// Delete removes the phase's WBS document (WBS 4.6 transactional
// deletion); ErrNotFound when the phase has none.
func (a wbsDocumentAdapter) Delete(ctx context.Context, phaseID string) error {
	doc, err := a.repo.GetByPhase(ctx, phaseID)
	if err != nil {
		return err
	}
	return a.repo.Delete(ctx, doc.ID)
}

func (a wbsDocumentAdapter) WithTx(tx persistence.Transaction) persistence.WBSDocumentRepository {
	return wbsDocumentAdapter{repo: a.repo.WithTx(tx)}
}

// NewReadinessService wires the readiness service to its repositories.
// lifecycle supplies the audited DRAFT → READY transition (WBS 3.7); it
// may be nil in unit scope, where MarkReady falls back to a direct status
// update.
func NewReadinessService(
	projects persistence.ProjectRepository,
	settings persistence.SettingsRepository,
	phases persistence.PhaseRepository,
	documents *dbrepo.DocumentRepository,
) *ReadinessService {
	return &ReadinessService{projects: projects, settings: settings, phases: phases, documents: wbsDocumentAdapter{documents}}
}

// WithLifecycle enables the audited readiness transition (the DRAFT →
// READY event is recorded through the lifecycle service).
func (s *ReadinessService) WithLifecycle(l *LifecycleService) *ReadinessService {
	s.lifecycle = l
	return s
}

// CheckReadiness evaluates every readiness requirement and returns the
// structured report. It never mutates project state: a failed check leaves
// the project exactly as found (T3.5.8).
func (s *ReadinessService) CheckReadiness(ctx context.Context, projectID string) (*ReadinessReport, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	rep := &ReadinessReport{}
	add := func(code string) { rep.Failures = append(rep.Failures, code) }

	// valid project
	p, err := s.projects.Get(ctx, parsed.String())
	if err != nil {
		return nil, translate(err)
	}
	// valid settings
	st, err := s.settings.Get(ctx, parsed.String())
	if err != nil {
		return nil, translate(err)
	}
	// worker/compression provider and model configured
	if st.WorkerProvider == "" {
		add(FailureWorkerProviderMissing)
	}
	if st.WorkerModel == "" {
		add(FailureWorkerModelMissing)
	}
	if st.CompressionProvider == "" {
		add(FailureCompressionProviderMissing)
	}
	if st.CompressionModel == "" {
		add(FailureCompressionModelMissing)
	}
	// valid context configuration (domain rules, T2.2.3–T2.2.6)
	if st.ContextWindow <= 0 || st.ContextThreshold <= 0 || st.ContextThreshold > 1 {
		add(FailureContextConfigurationInvalid)
	}
	// valid working directory (WBS 3.4 states): a file path is
	// not-a-directory; a nonexistent path is missing.
	wd := project.ValidateWorkingDirectory(st.WorkingDirectory)
	switch {
	case !wd.Configured:
		add(FailureWorkingDirectoryNotConfigured)
	case !wd.Valid:
		if _, statErr := os.Stat(st.WorkingDirectory); statErr == nil {
			add(FailureWorkingDirectoryNotADirectory)
		} else {
			add(FailureWorkingDirectoryMissing)
		}
	}
	// at least one phase
	phases, err := s.phases.ListByProject(ctx, parsed)
	if err != nil {
		return nil, translate(err)
	}
	if len(phases) == 0 {
		add(FailurePhaseMissing)
	} else {
		// current phase has WBS
		current, err := s.phases.GetNextIncomplete(ctx, parsed)
		if err != nil {
			return nil, translate(err)
		}
		if current == nil {
			// All phases DONE; nothing to execute.
			current = phases[len(phases)-1]
		}
		if _, err := s.documents.GetByPhase(ctx, current.ID.String()); err != nil {
			add(FailurePhaseWBSMissing)
		}
	}
	_ = p
	rep.Ready = len(rep.Failures) == 0
	return rep, nil
}

// MarkReady transitions a DRAFT project to READY when readiness holds
// (WBS 3.5, DRAFT → READY). A project that is not DRAFT, or whose
// readiness check fails, is left untouched; the failure is reported as
// ErrInvalidInput with the structured report.
func (s *ReadinessService) MarkReady(ctx context.Context, projectID string) (*ReadinessReport, error) {
	rep, err := s.CheckReadiness(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if !rep.Ready {
		return rep, ErrInvalidInput
	}
	parsed, _ := uuid.Parse(projectID)
	p, err := s.projects.Get(ctx, parsed.String())
	if err != nil {
		return nil, translate(err)
	}
	if p.Status != project.StatusDRAFT {
		return rep, ErrInvalidInput
	}
	if s.lifecycle != nil {
		// WBS 3.7: the status change and its audit event persist
		// atomically (reason "ready", distinct from the lifecycle
		// command reasons).
		if _, err := s.lifecycle.Transition(ctx, projectID, "ready", project.StatusREADY); err != nil {
			return nil, translate(err)
		}
		return rep, nil
	}
	p.Status = project.StatusREADY
	if err := s.projects.Update(ctx, p); err != nil {
		return nil, translate(err)
	}
	return rep, nil
}
