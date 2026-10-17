CREATE TABLE IF NOT EXISTS project_settings (
	project_id UUID PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
	working_directory TEXT NOT NULL,
	worker_provider TEXT NOT NULL DEFAULT '',
	worker_model TEXT NOT NULL DEFAULT '',
	compression_provider TEXT NOT NULL DEFAULT '',
	compression_model TEXT NOT NULL DEFAULT '',
	context_window INTEGER NOT NULL CHECK (context_window > 0),
	context_threshold DOUBLE PRECISION NOT NULL CHECK (context_threshold > 0 AND context_threshold <= 1),
	auto_continue BOOLEAN NOT NULL DEFAULT false,
	max_retries INTEGER NOT NULL DEFAULT 0 CHECK (max_retries >= 0),
	worker_timeout BIGINT NOT NULL CHECK (worker_timeout > 0),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
