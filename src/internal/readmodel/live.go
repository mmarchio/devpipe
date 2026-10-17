package readmodel

import (
	"time"

	"overseer/internal/execution"
)

// LiveEvent is the minimized SSE payload (WBS 17.75–17.78, 17.163): ids and
// status changes only — never prompts, logs or worker output. Clients use
// it as a signal to refetch authoritative read models.
type LiveEvent struct {
	ProjectSeq  int64             `json:"seq"`
	ExecutionID string            `json:"execution_id,omitempty"`
	Type        string            `json:"type"`
	Category    string            `json:"category"`
	Refs        map[string]string `json:"refs,omitempty"`
	Status      string            `json:"status,omitempty"`
	At          time.Time         `json:"at"`
}

// liveRefKeys are payload keys copied into Refs (ids only).
var liveRefKeys = []string{
	"work_unit_id", "unit_id", "phase_id", "plan_id", "dispatch_id", "validation_run_id", "run_id",
	"checkpoint_id", "continuation_id", "fix_attempt_id", "group_id", "pass_id", "step_seq",
}

// liveStatusKeys are payload keys whose short value is a status.
var liveStatusKeys = []string{"status", "outcome", "result", "token", "action", "scope", "kind", "to"}

// FromExecutionEvent minimizes a persisted event: only whitelisted id and
// status keys survive, each as a short string.
func FromExecutionEvent(projectSeq int64, e execution.Event) LiveEvent {
	le := LiveEvent{ProjectSeq: projectSeq, Type: string(e.Type), Category: string(execution.CategoryOf(e.Type)), At: e.CreatedAt.UTC()}
	if e.ExecutionID != nil {
		le.ExecutionID = e.ExecutionID.String()
	}
	for _, k := range liveRefKeys {
		if s, ok := shortScalar(e.Payload[k]); ok {
			if le.Refs == nil {
				le.Refs = map[string]string{}
			}
			le.Refs[k] = s
		}
	}
	for _, k := range liveStatusKeys {
		if s, ok := shortScalar(e.Payload[k]); ok {
			le.Status = s
			break
		}
	}
	return le
}

// shortScalar returns v as a string when it is a scalar of at most 64
// characters (ids, statuses, counts).
func shortScalar(v any) (string, bool) {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case float64:
		s = trimFloat(x)
	case int:
		s = itoa(int64(x))
	case int64:
		s = itoa(x)
	case bool:
		if x {
			s = "true"
		} else {
			s = "false"
		}
	default:
		return "", false
	}
	if s == "" || len(s) > 64 {
		return "", false
	}
	return s, true
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return itoa(int64(f))
	}
	return ""
}
