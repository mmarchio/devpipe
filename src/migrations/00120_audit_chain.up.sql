-- P18-56 (WBS 18.105–18.107): tamper-evident audit chain. Every event
-- belongs to a chain (one per project, "system" for project-less events)
-- and stores event_hash = sha256(prev_hash ‖ canonical(event)), computed by
-- the Go serializer (internal/audit). Rows written before this migration
-- carry their chain_key only; they are chained once (in seq order) by the
-- application under the chain's advisory lock before the chain is next
-- appended to (Decision 7).
ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS chain_key TEXT;
ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS chain_seq BIGINT;
ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS prev_hash TEXT;
ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS event_hash TEXT;

ALTER TABLE audit_events DISABLE TRIGGER audit_events_immutable;
UPDATE audit_events SET chain_key = CASE WHEN project_id IS NULL THEN 'system' ELSE 'project:' || project_id::text END
	WHERE chain_key IS NULL;
ALTER TABLE audit_events ENABLE TRIGGER audit_events_immutable;

ALTER TABLE audit_events ALTER COLUMN chain_key SET NOT NULL;
ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS audit_events_chain_key_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_chain_key_check
	CHECK (chain_key = CASE WHEN project_id IS NULL THEN 'system' ELSE 'project:' || project_id::text END);
ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS audit_events_chain_link_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_chain_link_check
	CHECK ((chain_seq IS NULL) = (event_hash IS NULL) AND (prev_hash IS NULL) = (event_hash IS NULL)
		AND (chain_seq IS NULL OR chain_seq >= 1)
		AND (event_hash IS NULL OR event_hash ~ '^[0-9a-f]{64}$')
		AND (prev_hash IS NULL OR prev_hash ~ '^[0-9a-f]{64}$'));
CREATE UNIQUE INDEX IF NOT EXISTS audit_events_chain_seq_key ON audit_events (chain_key, chain_seq);
CREATE INDEX IF NOT EXISTS audit_events_unchained_idx ON audit_events (chain_key, seq) WHERE event_hash IS NULL;

-- Rows stay immutable; the single exception is linking a not-yet-chained
-- row (setting chain_seq / prev_hash / event_hash once, nothing else).
CREATE OR REPLACE FUNCTION overseer_audit_events_guard() RETURNS trigger AS $$
DECLARE
	probe audit_events;
BEGIN
	IF TG_OP = 'UPDATE' AND OLD.event_hash IS NULL AND NEW.event_hash IS NOT NULL THEN
		probe := NEW;
		probe.chain_seq := NULL;
		probe.prev_hash := NULL;
		probe.event_hash := NULL;
		IF ROW(probe.*) IS NOT DISTINCT FROM ROW(OLD.*) THEN
			RETURN NEW;
		END IF;
	END IF;
	RAISE EXCEPTION 'audit_events rows are immutable' USING ERRCODE = 'check_violation';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS audit_events_immutable ON audit_events;
CREATE TRIGGER audit_events_immutable BEFORE UPDATE OR DELETE ON audit_events
	FOR EACH ROW EXECUTE FUNCTION overseer_audit_events_guard();

-- New rows must arrive linked: only rows that predate the chain may be
-- linked after the fact, so an unlinked row cannot be slipped in and then
-- legitimized by the next append.
CREATE OR REPLACE FUNCTION overseer_audit_events_require_link() RETURNS trigger AS $$
BEGIN
	IF NEW.event_hash IS NULL THEN
		RAISE EXCEPTION 'audit_events rows must be inserted with their chain link' USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS audit_events_require_link ON audit_events;
CREATE TRIGGER audit_events_require_link BEFORE INSERT ON audit_events
	FOR EACH ROW EXECUTE FUNCTION overseer_audit_events_require_link();
