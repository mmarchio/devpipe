package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/prompt"
)

// FragmentKind classifies a reusable static prompt fragment (WBS 10.46).
type FragmentKind string

const (
	FragmentSystemRolePrefix FragmentKind = "SYSTEM_ROLE_PREFIX"
	FragmentStatePrefix      FragmentKind = "STATE_PREFIX"
	FragmentStaticContext    FragmentKind = "STATIC_CONTEXT"
	FragmentCustom           FragmentKind = "CUSTOM"
)

// Known reports whether k is a valid kind.
func (k FragmentKind) Known() bool {
	switch k {
	case FragmentSystemRolePrefix, FragmentStatePrefix, FragmentStaticContext, FragmentCustom:
		return true
	}
	return false
}

// SourceRef is one source a fragment was built from.
type SourceRef struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Version int    `json:"version"`
	Hash    string `json:"hash"`
}

// PromptFragment is a reusable static prompt fragment (WBS 10.45/10.46).
// It is deliberately NOT a worker prompt: no conversion to a dispatchable
// prompt exists (T10.45.4) — a fragment can only feed composition.
type PromptFragment struct {
	ID        uuid.UUID
	ProjectID uuid.UUID

	FragmentKey string
	Kind        FragmentKind

	Content string
	Hash    string

	EstimatedTokens int

	Sources []SourceRef

	CreatedAt time.Time
}

// ErrInvalidFragment rejects an invalid fragment.
var ErrInvalidFragment = errors.New("cache: invalid prompt fragment")

// FragmentHash is the deterministic content hash (T10.46.2).
func FragmentHash(content string) string {
	s := sha256.Sum256([]byte("fragment\x00" + content))
	return hex.EncodeToString(s[:])
}

// FragmentKey is built ONLY from the dependencies of the fragment (WBS
// 10.47): its kind, project and the (type, id, version, hash) of the source
// components it contains — so a role prefix never changes because the
// ACTIVE_TASK changed.
func FragmentKey(projectID uuid.UUID, kind FragmentKind, sources []SourceRef) string {
	srcs := append([]SourceRef(nil), sources...)
	sort.Slice(srcs, func(i, j int) bool {
		a, b := srcs[i], srcs[j]
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Version < b.Version
	})
	buf := frame(nil, "project_id", []byte(projectID.String()))
	buf = frame(buf, "kind", []byte(kind))
	for _, s := range srcs {
		buf = frame(buf, "source", []byte(s.Type+"|"+s.ID))
		buf = frame(buf, "version", intBytes(s.Version))
		buf = frame(buf, "hash", []byte(s.Hash))
	}
	return "fragment:v1:" + sum(buf)
}

// fragmentMembers maps component types onto fragment kinds.
var fragmentMembers = map[FragmentKind]map[prompt.PromptComponentType]bool{
	FragmentSystemRolePrefix: {prompt.ComponentSystem: true, prompt.ComponentRoleInstruction: true},
	FragmentStatePrefix:      {prompt.ComponentStateInstruction: true, prompt.ComponentResponseProtocol: true},
	FragmentStaticContext:    {prompt.ComponentProjectContext: true},
}

// BuildFragments derives the static fragments of a composed prompt from
// its components (canonical order): the system+role prefix, the state
// prefix (state instruction + response protocol) and the static project
// context. Each fragment's key covers only its own components' provenance.
func BuildFragments(projectID uuid.UUID, comps prompt.Components, now time.Time) []PromptFragment {
	var out []PromptFragment
	for _, kind := range []FragmentKind{FragmentSystemRolePrefix, FragmentStatePrefix, FragmentStaticContext} {
		var parts []string
		var srcs []SourceRef
		for _, c := range comps.ByOrder() {
			if !fragmentMembers[kind][c.Type] || c.Content == "" {
				continue
			}
			parts = append(parts, c.Content)
			srcs = append(srcs, SourceRef{Type: string(c.Type), ID: c.SourceID.String(), Version: c.SourceVersion, Hash: componentHash(c)})
		}
		if len(parts) == 0 {
			continue
		}
		content := strings.Join(parts, "\n")
		out = append(out, PromptFragment{
			ID: uuid.New(), ProjectID: projectID, FragmentKey: FragmentKey(projectID, kind, srcs), Kind: kind,
			Content: content, Hash: FragmentHash(content), EstimatedTokens: prompt.EstimateContentTokens(content),
			Sources: srcs, CreatedAt: now,
		})
	}
	return out
}

// componentHash uses the source hash when present, else the content hash.
func componentHash(c prompt.PromptComponent) string {
	if c.SourceHash != "" {
		return c.SourceHash
	}
	return FragmentHash(c.Content)
}

// ValidateFragment checks a fragment (kind, key, project, hash = content).
func ValidateFragment(f PromptFragment) error {
	if f.ProjectID == uuid.Nil || !f.Kind.Known() || !strings.HasPrefix(f.FragmentKey, "fragment:") || f.Content == "" || f.Hash != FragmentHash(f.Content) {
		return ErrInvalidFragment
	}
	return nil
}
