-- Per-project formatting of the worker's replies to commands: Markdown
-- (default), XML or key:value. Existing rows keep Markdown.
ALTER TABLE project_worker_settings
	ADD COLUMN IF NOT EXISTS response_format TEXT NOT NULL DEFAULT 'MARKDOWN'
	CHECK (response_format IN ('MARKDOWN', 'XML', 'KEY_VALUE'));
