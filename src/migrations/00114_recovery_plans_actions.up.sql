-- P18-27 / P18-28 / P18-32 (WBS 18.8–18.11, 18.23, 18.182): persisted
-- recovery plans (written only when an action is taken or the startup
-- scan records a condition — inspecting a plan writes nothing), recovery
-- actions with an idempotency identity, and operator resolutions of
-- unknown deliveries (the dispatch row itself keeps DELIVERY_UNKNOWN).
CREATE TABLE IF NOT EXISTS recovery_plans (
	id UUID PRIMARY KEY,
	execution_id UUID NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	plan_hash TEXT NOT NULL CHECK (plan_hash ~ '^[0-9a-f]{64}$'),
	condition TEXT NOT NULL CHECK (condition <> ''),
	classification TEXT NOT NULL CHECK (classification IN ('SAFE_TO_RESUME', 'SAFE_TO_RETRY', 'RECONCILIATION_REQUIRED',
		'MANUAL_REVIEW_REQUIRED', 'TERMINAL')),
	plan JSONB NOT NULL,
	source TEXT NOT NULL CHECK (source IN ('STARTUP_SCAN', 'ACTION', 'ENGINE')),
	created_at TIMESTAMPTZ NOT NULL,
	UNIQUE (execution_id, plan_hash)
);
CREATE INDEX IF NOT EXISTS recovery_plans_execution_idx ON recovery_plans (execution_id, created_at);
DROP TRIGGER IF EXISTS recovery_plans_immutable ON recovery_plans;
CREATE TRIGGER recovery_plans_immutable BEFORE UPDATE OR DELETE ON recovery_plans
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();

CREATE TABLE IF NOT EXISTS recovery_actions (
	id UUID PRIMARY KEY,
	execution_id UUID NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	idempotency_key TEXT NOT NULL UNIQUE CHECK (length(idempotency_key) BETWEEN 8 AND 200),
	action TEXT NOT NULL CHECK (action <> ''),
	plan_hash TEXT NOT NULL DEFAULT '',
	evidence_hash TEXT NOT NULL DEFAULT '',
	actor_id TEXT NOT NULL CHECK (actor_id <> ''),
	actor_name TEXT NOT NULL DEFAULT '',
	reason TEXT NOT NULL DEFAULT '' CHECK (length(reason) <= 2000),
	confirmed BOOLEAN NOT NULL DEFAULT false,
	correlation_id TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK (status IN ('RUNNING', 'SUCCEEDED', 'FAILED')),
	result JSONB NOT NULL DEFAULT '{}',
	error TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	finished_at TIMESTAMPTZ NULL,
	CHECK ((status = 'RUNNING') = (finished_at IS NULL))
);
CREATE INDEX IF NOT EXISTS recovery_actions_execution_idx ON recovery_actions (execution_id, created_at);
CREATE OR REPLACE FUNCTION recovery_action_guard() RETURNS trigger AS $$
BEGIN
	IF TG_OP = 'DELETE' OR OLD.status <> 'RUNNING' THEN
		RAISE EXCEPTION 'recovery action % is final', OLD.status USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END $$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS recovery_actions_final ON recovery_actions;
CREATE TRIGGER recovery_actions_final BEFORE UPDATE OR DELETE ON recovery_actions
	FOR EACH ROW EXECUTE FUNCTION recovery_action_guard();

CREATE TABLE IF NOT EXISTS dispatch_resolutions (
	id UUID PRIMARY KEY,
	dispatch_id UUID NOT NULL UNIQUE REFERENCES worker_dispatches (id) ON DELETE CASCADE,
	execution_id UUID NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	resolution TEXT NOT NULL CHECK (resolution IN ('RESEND_AFTER_REVIEW', 'ACCEPT_REPOSITORY_STATE')),
	action_id UUID NOT NULL REFERENCES recovery_actions (id),
	actor_id TEXT NOT NULL CHECK (actor_id <> ''),
	reason TEXT NOT NULL CHECK (reason <> ''),
	evidence JSONB NOT NULL DEFAULT '{}',
	consumed_at TIMESTAMPTZ NULL,
	created_at TIMESTAMPTZ NOT NULL
);
