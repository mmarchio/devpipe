-- P13-21 (WBS 13.96): per-project worker response-protocol settings. Absent
-- row = defaults (1.0, STRICT, DETERMINISTIC_ONLY, default limits).
CREATE TABLE IF NOT EXISTS project_result_protocol_settings (
	project_id UUID PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
	protocol_major INTEGER NOT NULL CHECK (protocol_major > 0),
	protocol_minor INTEGER NOT NULL CHECK (protocol_minor >= 0),
	parse_mode TEXT NOT NULL CHECK (parse_mode IN ('STRICT', 'TOLERANT')),
	repair_mode TEXT NOT NULL CHECK (repair_mode IN ('NONE', 'DETERMINISTIC_ONLY', 'LLM_REPAIR')),
	max_response_bytes INTEGER NOT NULL CHECK (max_response_bytes > 0),
	max_summary_bytes INTEGER NOT NULL CHECK (max_summary_bytes > 0),
	max_item_bytes INTEGER NOT NULL CHECK (max_item_bytes > 0),
	max_next_action_bytes INTEGER NOT NULL CHECK (max_next_action_bytes > 0),
	max_completed_items INTEGER NOT NULL CHECK (max_completed_items > 0),
	max_remaining_items INTEGER NOT NULL CHECK (max_remaining_items > 0),
	max_blocking_items INTEGER NOT NULL CHECK (max_blocking_items > 0),
	max_artifact_items INTEGER NOT NULL CHECK (max_artifact_items >= 0),
	max_acceptance_items INTEGER NOT NULL CHECK (max_acceptance_items >= 0),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
