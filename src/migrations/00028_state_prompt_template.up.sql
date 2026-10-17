-- P6-10 (WBS 6.7): states reference a prompt-template key (stored, not
-- composed). An empty value is allowed only for control-only kinds (see
-- state.ValidateTemplate); action kinds must carry a template.
ALTER TABLE workflow_states
	ADD COLUMN prompt_template TEXT NOT NULL DEFAULT '';
