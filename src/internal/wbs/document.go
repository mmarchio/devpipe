package wbs

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// WBSDocument is the user-supplied statement of intended scope for a phase
// (WBS 2.9). It represents project intent — what should be built — as
// opposed to the generated Execution Plan. Content is retained verbatim so
// markdown formatting and code survive persistence. Version tracks content
// changes so later prompt-cache invalidation can detect drift.
type WBSDocument struct {
	ID      uuid.UUID
	PhaseID uuid.UUID

	Content string
	Version int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewDocument constructs a WBS document at version 1 (T2.9.1, T2.9.4).
// It requires a non-zero phase ID (T2.9.1) and non-blank content
// (T2.9.2).
func NewDocument(phaseID uuid.UUID, content string) (*WBSDocument, error) {
	if phaseID == uuid.Nil {
		return nil, fmt.Errorf("wbs: document requires a phase ID")
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("wbs: content must not be empty")
	}
	now := time.Now().UTC()
	return &WBSDocument{
		ID:        uuid.New(),
		PhaseID:   phaseID,
		Content:   content,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// Update replaces the content, increments the version, and refreshes
// UpdatedAt in UTC (T2.9.3–T2.9.5).
func (d *WBSDocument) Update(content string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("wbs: content must not be empty")
	}
	d.Content = content
	d.Version++
	d.UpdatedAt = time.Now().UTC()
	return nil
}

// SetVersion moves the version pointer to the given value. The version
// never moves backward (T2.9.6): a stale writer cannot overwrite a newer
// version.
func (d *WBSDocument) SetVersion(v int) error {
	if v < d.Version {
		return fmt.Errorf("wbs: version cannot move backward (current %d, requested %d)", d.Version, v)
	}
	d.Version = v
	return nil
}
