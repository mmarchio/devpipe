package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"overseer/internal/health"
	"overseer/internal/persistence"
)

// DiagnosticsStore reads the durable diagnostic facts.
type DiagnosticsStore interface {
	System(ctx context.Context, limit int) (*persistence.SystemDiagnostics, error)
	Project(ctx context.Context, projectID uuid.UUID, limit int) (*persistence.ProjectDiagnostics, error)
}

// DiagnosticsDeps are the bundle sources (nil members are omitted).
type DiagnosticsDeps struct {
	Store        DiagnosticsStore
	Health       *SystemHealthService
	Alerts       *AlertService
	Audit        *AuditVerificationService
	Gate         *WorkGate
	Disk         *DiskMonitor
	RecentErrors func() []string
	// Redact removes configured secret values from free text.
	Redact         func(any) any
	Version        string
	Profile        string
	SchemaVersion  int
	InstanceID     string
	ConfigHash     string
	ConfigSections map[string]string
	Now            func() time.Time
}

// MaxBundleBytes caps a diagnostic bundle (WBS 18.187).
const MaxBundleBytes = 1 << 20

// SystemBundle is the system diagnostic bundle (WBS 18.185).
type SystemBundle struct {
	GeneratedAt    time.Time                      `json:"generated_at"`
	Version        string                         `json:"version"`
	Profile        string                         `json:"profile"`
	SchemaVersion  int                            `json:"schema_version"`
	InstanceID     string                         `json:"instance_id"`
	ConfigHash     string                         `json:"config_hash"`
	ConfigSections map[string]string              `json:"config_section_hashes"`
	Health         *health.Report                 `json:"health,omitempty"`
	Alerts         []health.Alert                 `json:"alerts"`
	Audit          *AuditVerification             `json:"audit_verification,omitempty"`
	NewWorkBlocked map[string]string              `json:"new_work_blocked"`
	Disk           []DiskUsage                    `json:"disk"`
	RecentErrors   []string                       `json:"recent_errors"`
	Facts          *persistence.SystemDiagnostics `json:"facts"`
}

// ProjectBundle is one project's diagnostic bundle (WBS 18.186 / 18.187).
type ProjectBundle struct {
	GeneratedAt time.Time                       `json:"generated_at"`
	Facts       *persistence.ProjectDiagnostics `json:"facts"`
	// Lineage of the project's DONE units would repeat the facts; it is
	// referenced per unit instead.
	LineagePaths []string `json:"lineage_paths"`
	Truncated    bool     `json:"truncated"`
	Bytes        int      `json:"bytes"`
}

// DiagnosticsService assembles diagnostic bundles — identifiers, states,
// hashes and redacted recent errors only; it never mutates anything.
type DiagnosticsService struct{ d DiagnosticsDeps }

// NewDiagnosticsService builds the service.
func NewDiagnosticsService(d DiagnosticsDeps) *DiagnosticsService {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &DiagnosticsService{d: d}
}

// System returns the system bundle.
func (s *DiagnosticsService) System(ctx context.Context) (*SystemBundle, error) {
	facts, err := s.d.Store.System(ctx, 200)
	if err != nil {
		return nil, translate(err)
	}
	b := &SystemBundle{GeneratedAt: s.d.Now().UTC(), Version: s.d.Version, Profile: s.d.Profile, SchemaVersion: s.d.SchemaVersion,
		InstanceID: s.d.InstanceID, ConfigHash: s.d.ConfigHash, ConfigSections: s.d.ConfigSections, Alerts: []health.Alert{},
		NewWorkBlocked: s.d.Gate.Blocks(), Disk: []DiskUsage{}, RecentErrors: []string{}, Facts: facts}
	if s.d.Health != nil {
		rep := s.d.Health.Report(ctx)
		b.Health = &rep
	}
	if s.d.Alerts != nil {
		for _, a := range s.d.Alerts.Evaluate(ctx) {
			if a.Active {
				b.Alerts = append(b.Alerts, a)
			}
		}
	}
	if s.d.Audit != nil {
		b.Audit = s.d.Audit.Last()
	}
	if s.d.Disk != nil {
		b.Disk = append(b.Disk, s.d.Disk.Last()...)
	}
	if s.d.RecentErrors != nil {
		b.RecentErrors = append(b.RecentErrors, s.d.RecentErrors()...)
	}
	if s.d.Redact != nil {
		for i, e := range b.RecentErrors {
			if v, ok := s.d.Redact(e).(string); ok {
				b.RecentErrors[i] = v
			}
		}
	}
	return b, nil
}

// Project returns a project bundle within MaxBundleBytes: lists are
// shortened until it fits (Truncated reports it).
func (s *DiagnosticsService) Project(ctx context.Context, projectID uuid.UUID) (*ProjectBundle, error) {
	for limit := 200; ; limit /= 2 {
		facts, err := s.d.Store.Project(ctx, projectID, limit)
		if err != nil {
			return nil, translate(err)
		}
		b := &ProjectBundle{GeneratedAt: s.d.Now().UTC(), Facts: facts, LineagePaths: []string{}, Truncated: limit < 200}
		for _, u := range facts.Units {
			if st, _ := u["status"].(string); st == "DONE" {
				if id, _ := u["id"].(string); id != "" {
					b.LineagePaths = append(b.LineagePaths, "/api/v1/projects/"+projectID.String()+"/work-units/"+id+"/lineage")
				}
			}
		}
		raw, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		b.Bytes = len(raw)
		if len(raw) <= MaxBundleBytes || limit <= 1 {
			return b, nil
		}
	}
}
