package workerresult

// AttemptOutcome is a parse attempt outcome.
type AttemptOutcome string

// Attempt outcomes.
const (
	OutcomeAccepted  AttemptOutcome = "ACCEPTED"
	OutcomeRejected  AttemptOutcome = "REJECTED"
	OutcomeDuplicate AttemptOutcome = "DUPLICATE"
)

// AttemptRecord is what compliance metrics aggregate over; grouping keys
// come from the authoritative dispatch binding (WBS 13.63).
type AttemptRecord struct {
	Outcome      AttemptOutcome
	ErrorCode    WorkerResultErrorCode
	ResultStatus WorkerResultStatus
	Repaired     bool
	Reparse      bool

	ProjectID string
	Role      string
	State     string
	Provider  string
	Model     string
}

// Compliance are protocol compliance counters (WBS 13.62).
type Compliance struct {
	Attempts           int
	Parsed             int
	Missing            int
	Malformed          int
	Ambiguous          int
	SchemaInvalid      int
	SemanticInvalid    int
	UnsupportedVersion int
	Duplicates         int
	RepairAttempts     int
	RepairSuccesses    int
	// ReportedFailed counts valid results whose worker status is failed —
	// distinct from protocol failures (WBS 13.32.3).
	ReportedFailed int
	ComplianceRate float64
}

// Aggregate counts each attempt exactly once. Duplicate (re-)parses of an
// already accepted result are counted separately and excluded from the
// compliance rate.
func Aggregate(records []AttemptRecord) Compliance {
	var c Compliance
	for _, r := range records {
		c.Attempts++
		if r.Repaired {
			c.RepairAttempts++
		}
		switch r.Outcome {
		case OutcomeDuplicate:
			c.Duplicates++
			continue
		case OutcomeAccepted:
			c.Parsed++
			if r.Repaired {
				c.RepairSuccesses++
			}
			if r.ResultStatus == StatusFailed {
				c.ReportedFailed++
			}
			continue
		}
		switch r.ErrorCode {
		case ResultMissing:
			c.Missing++
		case ResultMalformed:
			c.Malformed++
		case ResultAmbiguous:
			c.Ambiguous++
		case ResultSchemaInvalid:
			c.SchemaInvalid++
		case ResultSemanticInvalid:
			c.SemanticInvalid++
		case ResultUnsupportedVersion:
			c.UnsupportedVersion++
		}
	}
	if n := c.Attempts - c.Duplicates; n > 0 {
		c.ComplianceRate = float64(c.Parsed) / float64(n)
	}
	return c
}

// GroupBy names a compliance grouping dimension.
type GroupBy string

// Grouping dimensions.
const (
	ByRole     GroupBy = "role"
	ByState    GroupBy = "state"
	ByProvider GroupBy = "provider"
	ByModel    GroupBy = "model"
	ByProject  GroupBy = "project"
)

// AggregateBy groups compliance by one dimension.
func AggregateBy(records []AttemptRecord, by GroupBy) map[string]Compliance {
	groups := map[string][]AttemptRecord{}
	for _, r := range records {
		var k string
		switch by {
		case ByRole:
			k = r.Role
		case ByState:
			k = r.State
		case ByProvider:
			k = r.Provider
		case ByModel:
			k = r.Model
		default:
			k = r.ProjectID
		}
		groups[k] = append(groups[k], r)
	}
	out := map[string]Compliance{}
	for k, rs := range groups {
		out[k] = Aggregate(rs)
	}
	return out
}
