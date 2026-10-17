package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"overseer/internal/audit"
)

// AuditChainStore is the persistence the audit verifier needs.
type AuditChainStore interface {
	VerifyAuditChains(ctx context.Context, chainKey string) ([]audit.ChainResult, error)
	AuditFailureAcknowledged(ctx context.Context, fingerprint string) (bool, error)
	AcknowledgeAuditFailure(ctx context.Context, ack *audit.FailureAck) error
}

// AuditVerification is one verification of the audit hash chains.
type AuditVerification struct {
	Status audit.VerifyStatus `json:"status"`
	// Scope is "all" or one chain key.
	Scope  string              `json:"scope"`
	Chains []audit.ChainResult `json:"chains"`
	// Fingerprint identifies the failure (empty when VALID).
	Fingerprint string `json:"fingerprint,omitempty"`
	// Acknowledged: an operator acknowledged exactly this failure.
	Acknowledged bool `json:"acknowledged"`
	// BlocksNewWork: the failure is unacknowledged (decision U9).
	BlocksNewWork bool      `json:"blocks_new_work"`
	VerifiedAt    time.Time `json:"verified_at"`
}

// AlertAuditVerificationFailed is the alert raised for an unacknowledged
// INVALID / INCOMPLETE verification (WBS 18.110).
const AlertAuditVerificationFailed = "AUDIT_VERIFICATION_FAILED"

// AuditVerificationService verifies the tamper-evident audit chains
// (WBS 18.108–18.110). An INVALID or INCOMPLETE result is a security
// incident (decision U9): AUDIT_VERIFICATION_FAILED is logged, readiness
// reports NOT_READY and the work gate blocks new work until an operator
// acknowledges that exact failure (an audited ACKNOWLEDGE_AUDIT_FAILURE
// action with a reason). Verification itself never writes.
type AuditVerificationService struct {
	store AuditChainStore
	gate  *WorkGate
	log   *slog.Logger
	now   func() time.Time

	mu   sync.Mutex
	last *AuditVerification
}

// NewAuditVerificationService builds the service; gate may be nil.
func NewAuditVerificationService(store AuditChainStore, gate *WorkGate, log *slog.Logger) *AuditVerificationService {
	if log == nil {
		log = slog.Default()
	}
	return &AuditVerificationService{store: store, gate: gate, log: log, now: time.Now}
}

// Verify verifies every chain (project nil) or one project's chain.
func (s *AuditVerificationService) Verify(ctx context.Context, project *uuid.UUID) (*AuditVerification, error) {
	key, scope := "", "all"
	if project != nil {
		key = audit.ChainKey(project)
		scope = key
	}
	results, err := s.store.VerifyAuditChains(ctx, key)
	if err != nil {
		return nil, translate(err)
	}
	v := &AuditVerification{Status: audit.Overall(results), Scope: scope, Chains: results, VerifiedAt: s.now().UTC()}
	if v.Chains == nil {
		v.Chains = []audit.ChainResult{}
	}
	if v.Fingerprint = audit.Fingerprint(results); v.Fingerprint != "" {
		if v.Acknowledged, err = s.store.AuditFailureAcknowledged(ctx, v.Fingerprint); err != nil {
			return nil, translate(err)
		}
		v.BlocksNewWork = !v.Acknowledged
	}
	s.apply(v)
	return v, nil
}

// apply updates the gate and the latest full result.
func (s *AuditVerificationService) apply(v *AuditVerification) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.BlocksNewWork {
		reason := "audit verification " + string(v.Status) + ": " + describeFailures(v.Chains) +
			"; acknowledge the failure (ACKNOWLEDGE_AUDIT_FAILURE) to allow new work"
		s.gate.Block(GateAuditVerification, reason)
		s.log.Error("audit verification failed", "alert", AlertAuditVerificationFailed, "status", string(v.Status),
			"scope", v.Scope, "fingerprint", v.Fingerprint, "detail", describeFailures(v.Chains))
	} else if v.Scope == "all" {
		s.gate.Clear(GateAuditVerification)
	}
	if v.Scope == "all" || v.BlocksNewWork || s.last == nil {
		cp := *v
		s.last = &cp
	}
}

func describeFailures(results []audit.ChainResult) string {
	var parts []string
	for _, r := range audit.Failures(results) {
		p := fmt.Sprintf("chain %s %s at seq %d (%s)", r.Chain, r.Status, r.FirstFailing, r.FailureReason)
		for _, g := range r.Gaps {
			p += fmt.Sprintf(" gap %d-%d", g.From, g.To)
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}

// Last returns the latest verification (nil before the first).
func (s *AuditVerificationService) Last() *AuditVerification {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		return nil
	}
	cp := *s.last
	return &cp
}

// AcknowledgeRequest is an operator's acknowledgement.
type AcknowledgeRequest struct {
	Reason string `json:"reason"`
	// Fingerprint, when given, must match the current failure (the
	// operator acknowledges what they saw).
	Fingerprint string `json:"fingerprint"`
	By          string `json:"-"`
	ByName      string `json:"-"`
	RequestID   string `json:"-"`
}

// AcknowledgeResult is the outcome of an acknowledgement.
type AcknowledgeResult struct {
	Acknowledgement audit.FailureAck   `json:"acknowledgement"`
	Verification    *AuditVerification `json:"verification"`
}

// ErrNothingToAcknowledge: every chain verifies.
var ErrNothingToAcknowledge = fmt.Errorf("%w: audit verification is VALID; nothing to acknowledge", ErrConflict)

// Acknowledge re-verifies every chain and records the operator's
// acknowledgement of the current failure, lifting the work-gate block.
// History is not changed: the failure stays visible (INVALID /
// INCOMPLETE, acknowledged).
func (s *AuditVerificationService) Acknowledge(ctx context.Context, req AcknowledgeRequest) (*AcknowledgeResult, error) {
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" || len(req.Reason) > audit.MaxReason {
		return nil, fmt.Errorf("%w: a reason (1–%d characters) is required", ErrInvalidInput, audit.MaxReason)
	}
	if req.By == "" {
		return nil, fmt.Errorf("%w: acknowledging actor unknown", ErrInvalidInput)
	}
	results, err := s.store.VerifyAuditChains(ctx, "")
	if err != nil {
		return nil, translate(err)
	}
	fp := audit.Fingerprint(results)
	if fp == "" {
		return nil, ErrNothingToAcknowledge
	}
	if req.Fingerprint != "" && req.Fingerprint != fp {
		return nil, fmt.Errorf("%w: the audit failure changed since it was viewed (fingerprint %s, now %s); review it again", ErrConflict, req.Fingerprint, fp)
	}
	ack := &audit.FailureAck{ID: uuid.New(), Fingerprint: fp, Status: audit.Overall(results), Failure: audit.Failures(results),
		Reason: req.Reason, By: req.By, ByName: req.ByName, RequestID: req.RequestID, At: s.now().UTC()}
	if err := s.store.AcknowledgeAuditFailure(ctx, ack); err != nil {
		return nil, translate(err)
	}
	s.log.Warn("audit verification failure acknowledged", "fingerprint", fp, "status", string(ack.Status), "by", ack.By, "reason", ack.Reason)
	v := &AuditVerification{Status: audit.Overall(results), Scope: "all", Chains: results, Fingerprint: fp, Acknowledged: true, VerifiedAt: s.now().UTC()}
	s.apply(v)
	return &AcknowledgeResult{Acknowledgement: *ack, Verification: v}, nil
}
