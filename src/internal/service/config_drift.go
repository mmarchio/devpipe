package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/hardening"
	"overseer/internal/persistence"
	"overseer/internal/project"
	"overseer/internal/readmodel"
)

type driftExecutions interface {
	GetActive(ctx context.Context, ex persistence.Executor, projectID uuid.UUID) (*persistence.ExecutionRecord, error)
	GetSnapshot(ctx context.Context, executionID uuid.UUID) (*persistence.ConfigurationSnapshotRecord, error)
}

type driftSettings interface {
	Get(ctx context.Context, projectID string) (*project.Settings, error)
}

type driftEngine interface {
	EngineSettings(ctx context.Context, projectID uuid.UUID) (execution.EngineSettings, hardening.Settings, bool, error)
}

// ConfigDriftService reports configuration that changed after an active
// execution captured its configuration (WBS 18.4). The execution keeps
// running on its captured snapshot; drift is surfaced, never applied.
type ConfigDriftService struct {
	execs    driftExecutions
	snaps    persistence.AppConfigSnapshotRepository
	settings driftSettings
	engine   driftEngine
	current  config.Snapshot
}

// NewConfigDriftService builds the service; current is this process's
// startup snapshot.
func NewConfigDriftService(execs driftExecutions, snaps persistence.AppConfigSnapshotRepository, settings driftSettings, engine driftEngine,
	current config.Snapshot) *ConfigDriftService {
	return &ConfigDriftService{execs: execs, snaps: snaps, settings: settings, engine: engine, current: current}
}

func specHash(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ForProject returns the drift of the project's active execution (empty
// when none is active).
func (s *ConfigDriftService) ForProject(ctx context.Context, projectID uuid.UUID) (readmodel.ConfigDrift, error) {
	out := readmodel.ConfigDrift{Items: []readmodel.DriftItem{}}
	rec, err := s.execs.GetActive(ctx, nil, projectID)
	if errors.Is(err, persistence.ErrNotFound) {
		return out, nil
	} else if err != nil {
		return out, translate(err)
	}
	out.ExecutionID = rec.ID.String()
	// Application configuration (process-level sections).
	if rec.AppConfigHash != "" && rec.AppConfigHash != s.current.Hash && s.snaps != nil {
		if prev, err := s.snaps.ByHash(ctx, rec.AppConfigHash); err == nil {
			changed := config.ChangedSections(prev.Sections, s.current.Sections)
			for _, sec := range config.DriftRelevantSections {
				for _, c := range changed {
					if c == sec {
						out.Items = append(out.Items, readmodel.DriftItem{Source: "application", Section: sec,
							Detail: "application " + sec + " configuration changed since the execution started"})
					}
				}
			}
		}
	}
	// Project-level configuration vs the captured execution snapshot.
	snap, err := s.execs.GetSnapshot(ctx, rec.ID)
	if err != nil {
		return out, translate(err)
	}
	content, err := execution.DecodeContent(snap.Canonical)
	if err != nil {
		return out, fmt.Errorf("%w: execution snapshot unreadable", ErrInfrastructure)
	}
	add := func(section, detail string) {
		out.Items = append(out.Items, readmodel.DriftItem{Source: "project", Section: section, Detail: detail})
	}
	if s.settings != nil {
		if st, err := s.settings.Get(ctx, projectID.String()); err == nil {
			if st.WorkerProvider != content.Worker.Provider || st.WorkerModel != content.Worker.Model {
				add("worker", fmt.Sprintf("worker %s/%s now configured; the execution uses %s/%s", st.WorkerProvider, st.WorkerModel,
					content.Worker.Provider, content.Worker.Model))
			}
			if st.WorkingDirectory != content.WorkingDirectory {
				add("repository", "working directory changed; the execution uses "+content.WorkingDirectory)
			}
		}
	}
	if s.engine != nil {
		if cur, _, _, err := s.engine.EngineSettings(ctx, projectID); err == nil {
			cap := content.EngineSettingsOrDefault()
			if cur.CheckpointPolicy != cap.CheckpointPolicy {
				add("checkpoint_policy", fmt.Sprintf("checkpoint policy now %s; the execution uses %s", cur.CheckpointPolicy, cap.CheckpointPolicy))
			}
			if specHash([]any{cur.UnitValidation, cur.PhaseValidation, cur.FinalValidation}) != specHash([]any{cap.UnitValidation, cap.PhaseValidation, cap.FinalValidation}) {
				add("validation_policy", "validation commands changed; the execution uses its captured commands")
			}
			if cur.ValidationTimeout != cap.ValidationTimeout || cur.OutputCapBytes != cap.OutputCapBytes {
				add("resource_limits", "validation timeout / output limits changed; the execution uses its captured limits")
			}
		}
	}
	sort.SliceStable(out.Items, func(i, j int) bool {
		if out.Items[i].Source != out.Items[j].Source {
			return out.Items[i].Source < out.Items[j].Source
		}
		return out.Items[i].Section < out.Items[j].Section
	})
	return out, nil
}
