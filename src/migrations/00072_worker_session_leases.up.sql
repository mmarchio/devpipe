-- P12-48 (WBS 12.57/12.80): DB-backed ownership of a conversational
-- session: one dispatch (on one overseer instance) at a time. An expired
-- lease is reclaimable, so a crash never locks a session permanently.
CREATE TABLE IF NOT EXISTS worker_session_leases (
	session_id UUID PRIMARY KEY REFERENCES worker_sessions (id) ON DELETE CASCADE,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	owner TEXT NOT NULL CHECK (owner <> ''),
	dispatch_id UUID NULL,
	acquired_at TIMESTAMPTZ NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL,
	CHECK (expires_at > acquired_at)
);
