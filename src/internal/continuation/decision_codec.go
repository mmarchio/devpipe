package continuation

import (
	"encoding/json"

	"overseer/internal/worker"
)

type decisionJSON struct {
	Strategy      Strategy           `json:"strategy"`
	Reason        DecisionReason     `json:"reason"`
	ContextTokens *int64             `json:"context_tokens"`
	ContextWindow *int64             `json:"context_window"`
	Utilization   *float64           `json:"utilization"`
	Threshold     float64            `json:"threshold"`
	Estimated     bool               `json:"estimated"`
	Source        worker.UsageSource `json:"source"`
}

// EncodeDecision is the byte-stable persisted form of a decision.
func EncodeDecision(d Decision) []byte {
	b, _ := json.Marshal(decisionJSON(d))
	return b
}

// DecodeDecision reconstructs a persisted decision exactly (WBS 14.15).
func DecodeDecision(b []byte) (Decision, error) {
	var dj decisionJSON
	if err := json.Unmarshal(b, &dj); err != nil {
		return Decision{}, err
	}
	if _, err := ParseStrategy(string(dj.Strategy)); err != nil {
		return Decision{}, err
	}
	if _, err := ParseDecisionReason(string(dj.Reason)); err != nil {
		return Decision{}, err
	}
	return Decision(dj), nil
}
