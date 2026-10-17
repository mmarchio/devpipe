package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

import "github.com/google/uuid"

// ContentHash is the deterministic content-addressing function for
// template version rows (WBS 8.4 "content_hash" field, T8.4.6): the hex
// SHA-256 of the stored source bytes. Identical bytes always yield the
// same hash; a single byte of difference changes it.
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// TemplateVersion is one immutable, append-only content revision of a
// template (WBS 8.4: template_id, version, content, content_hash,
// created_at). Historical rows are never overwritten.
type TemplateVersion struct {
	TemplateID  uuid.UUID
	Version     int
	Content     string
	ContentHash string
	CreatedAt   time.Time
}

// TemplateVersionSeries is the in-domain (pure, no DB) model of a
// template's version history (WBS 8.3 + the pure half of WBS 8.4): it
// stores template SOURCE exactly — multiline, unicode, and template
// syntax are all preserved byte-for-byte (T8.3.1–T8.3.3) — and versions
// it by appending instead of overwriting (T8.4.1–T8.4.4, T8.4.6). The
// DB persistence half (concurrent append, MAX+1 allocation) lives in the
// repository tasks; this series defines the semantics they mirror.
type TemplateVersionSeries struct {
	TemplateID uuid.UUID
	rows       []TemplateVersion
}

// NewVersionSeries validates the initial content (non-empty, T8.3.4) and
// creates the v1 row (T8.4.1).
func NewVersionSeries(templateID uuid.UUID, content string, ts time.Time) (*TemplateVersionSeries, error) {
	if content == "" {
		return nil, fmt.Errorf("prompt: template content must not be empty")
	}
	return &TemplateVersionSeries{
		TemplateID: templateID,
		rows:       []TemplateVersion{newRow(templateID, 1, content, ts)},
	}, nil
}

// Append records a new content revision (T8.4.2) and advances the
// current version (T8.4.4). Empty content is rejected (T8.3.4).
func (s *TemplateVersionSeries) Append(content string, ts time.Time) (*TemplateVersion, error) {
	if content == "" {
		return nil, fmt.Errorf("prompt: template content must not be empty")
	}
	next := s.rows[len(s.rows)-1].Version + 1
	row := newRow(s.TemplateID, next, content, ts)
	s.rows = append(s.rows, row)
	return &s.rows[len(s.rows)-1], nil
}

// GetVersion retrieves any historical revision (T8.4.3) by exact version.
func (s *TemplateVersionSeries) GetVersion(version int) (*TemplateVersion, error) {
	for i := range s.rows {
		if s.rows[i].Version == version {
			return &s.rows[i], nil
		}
	}
	return nil, fmt.Errorf("prompt: version %d not found", version)
}

// Current returns the highest version row (T8.4.4).
func (s *TemplateVersionSeries) Current() (*TemplateVersion, error) {
	if len(s.rows) == 0 {
		return nil, fmt.Errorf("prompt: series has no versions")
	}
	return &s.rows[len(s.rows)-1], nil
}

// Len reports the number of stored revisions.
func (s *TemplateVersionSeries) Len() int {
	return len(s.rows)
}

func newRow(templateID uuid.UUID, version int, content string, ts time.Time) TemplateVersion {
	return TemplateVersion{
		TemplateID:  templateID,
		Version:     version,
		Content:     content,
		ContentHash: ContentHash(content),
		CreatedAt:   ts,
	}
}
