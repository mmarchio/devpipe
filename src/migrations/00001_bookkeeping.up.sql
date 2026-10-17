CREATE TABLE IF NOT EXISTS overseer_bookkeeping (
	version INT PRIMARY KEY,
	name TEXT NOT NULL,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
