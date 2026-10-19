# Overseer metrics

Generated from the registered catalog (`app.MetricsCatalog`); `GET /metrics` exposes these series in the Prometheus text format.
Durable series are recomputed from database rows at every scrape (restart-safe, no double counting); `overseer_http_requests_total` and
`overseer_database_pool_connections` are per process. Labels never carry identifiers.

| Metric | Type | Category | Labels | Description |
| --- | --- | --- | --- | --- |
| `overseer_alert_active` | gauge | recovery | alert | 1 while an alert condition is active (WBS 18.118). |
| `overseer_compression_runs_total` | counter | compression | outcome | Compression runs by outcome. |
| `overseer_database_pool_connections` | gauge | database | state | Database pool connections of this process by state. |
| `overseer_database_size_bytes` | gauge | database |  | Size of the overseer database. |
| `overseer_execution_steps_total` | counter | scheduler | action, status | Engine steps by action and outcome. |
| `overseer_executions` | gauge | workflow | status | Executions by status. |
| `overseer_http_requests_total` | counter | http | method, class | HTTP requests served by this process, by method and status class. |
| `overseer_metrics_label_rejections_total` | counter | http | metric | Label values rejected as high-cardinality or undeclared. |
| `overseer_precompute_jobs_total` | counter | precompute | status | Precompute jobs by status. |
| `overseer_projects` | gauge | workflow | status | Projects by status. |
| `overseer_prompt_cache_entries` | gauge | cache |  | Prompt cache entries. |
| `overseer_prompt_cache_hits_total` | counter | cache |  | Prompt cache hits. |
| `overseer_recovery_action_failures_total` | counter | recovery |  | Operator recovery actions that failed. |
| `overseer_recovery_entries_total` | counter | recovery |  | Times executions entered recovery. |
| `overseer_recovery_oldest_seconds` | gauge | recovery |  | Time the longest-waiting execution has spent in recovery. |
| `overseer_recovery_required_executions` | gauge | recovery | condition | Executions in RECOVERY_REQUIRED by condition. |
| `overseer_recovery_resolutions_total` | counter | recovery | mode | Recoveries resolved, automatically (engine) or manually (operator action). |
| `overseer_repository_checkpoints_total` | counter | repository | kind, status | Repository checkpoints by kind and status. |
| `overseer_scheduler_waiting_executions` | gauge | scheduler | blocked | Executions waiting, by whether new work is blocked. |
| `overseer_storage_bytes` | gauge | database | category | Database storage by category (WBS 18.97). |
| `overseer_transition_latency_seconds` | histogram | precompute | precompute | Worker result → next worker prompt ready, by precompute hit / miss. |
| `overseer_validation_command_duration_seconds` | histogram | validation | outcome | Validation command duration by outcome. |
| `overseer_validation_commands_total` | counter | validation | outcome | Validation commands by outcome. |
| `overseer_validation_run_duration_seconds` | histogram | validation | status | Validation run duration by status. |
| `overseer_validation_runs_total` | counter | validation | status | Validation runs by status. |
| `overseer_work_units` | gauge | workflow | status | Work units by status. |
| `overseer_worker_dispatches_total` | counter | worker | outcome | Worker dispatches by outcome (timeouts distinct). |
| `overseer_worker_prompts_total` | counter | prompt | origin | Worker prompts prepared, by origin (precomputed projection or live). |
| `overseer_worker_request_duration_seconds` | histogram | worker | outcome | Worker request → response latency by outcome (timeouts distinct). |
| `overseer_worker_timeouts_total` | counter | worker |  | Worker requests that timed out. |
