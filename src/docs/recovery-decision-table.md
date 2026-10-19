# Recovery decision table

Generated from `internal/recovery` (`DecisionTable`, `SideEffectMatrix`, the condition rules); do not edit by hand.

## Situations

| Condition | Recovery condition | Auto Retry | Auto Resume | Human Review | Retry policy |
| --- | --- | --- | --- | --- | --- |
| Worker connection failed before send | DISPATCH_NOT_SENT | Yes | Yes | No | worker_connect |
| Delivery confirmed | INFLIGHT_DISPATCH | No resend | Wait | Maybe | — |
| Delivery unknown | DELIVERY_UNKNOWN | No | No | Yes/reconcile | — |
| Validation test failed | — | No infra retry | FIX | No | — |
| Validation infrastructure failed | VALIDATION_INFRA | Yes, bounded | Yes | Maybe | validation_infra |
| Required checkpoint failed | PENDING_CHECKPOINT | Yes if safely repeatable | No DONE | Maybe | git_lock |
| DB unavailable | — | No side effects | After restore | Maybe | db_tx |

## Side effects

| Operation | Idempotent | Observable | Reconcilable | Safe to retry | Ambiguity behavior |
| --- | --- | --- | --- | --- | --- |
| Worker dispatch | no | yes | yes | ONLY_IF_PROVABLY_NOT_PERFORMED | DELIVERY_UNKNOWN: stop, capture evidence, Phase 12 reconciliation, explicit operator resolution; never resent automatically |
| Compression call | yes | no | no | ALWAYS | No external state; a lost response is recomputed or the canonical prompt is used when it fits |
| Validation command | yes | yes | yes | ALWAYS | An interrupted run is ABANDONED (never PASS / FAIL) and rerun |
| Git checkpoint | yes | yes | yes | ONLY_IF_PROVABLY_NOT_PERFORMED | Trailer lookup + parent / tree verification; found → adopt, uncertain → CHECKPOINT_AMBIGUOUS recovery |
| Filesystem materialization | yes | yes | yes | ALWAYS | Atomic temp-file + rename: the original or the full replacement exists; derived files never become authority |

## Conditions

| Condition | Classification | Safe actions | Unsafe actions | Automatic | Human approval |
| --- | --- | --- | --- | --- | --- |
| `ARTIFACT_CORRUPT` | MANUAL_REVIEW_REQUIRED | `MANUAL_RESUME` | `RESEND` | — | yes |
| `CHECKPOINT_AMBIGUOUS` | MANUAL_REVIEW_REQUIRED | `ACCEPT_REPOSITORY_HEAD`, `MANUAL_RESUME` | `RETRY_CHECKPOINT` | — | yes |
| `CHECKPOINT_REJECTED` | MANUAL_REVIEW_REQUIRED | `MANUAL_RESUME`, `RETRY_CHECKPOINT` | — | — | yes |
| `CONFIG_DRIFT` | SAFE_TO_RESUME | `RESUME` | — | `RESUME` | no |
| `DELIVERY_UNKNOWN` | RECONCILIATION_REQUIRED | `RECONCILE_DISPATCH`, `RESOLVE_DELIVERY`, `VALIDATE_REPOSITORY_STATE` | `MANUAL_RESUME`, `RESEND` | — | yes |
| `DISK_FULL` | MANUAL_REVIEW_REQUIRED | `MANUAL_RESUME` | — | — | yes |
| `DISPATCH_NOT_SENT` | SAFE_TO_RETRY | `RESEND` | — | `RESEND` | no |
| `EXECUTION_TERMINAL` | TERMINAL | — | — | — | no |
| `EXPIRED_LEASE` | SAFE_TO_RESUME | `RESUME` | — | `RESUME` | no |
| `INFLIGHT_DISPATCH` | RECONCILIATION_REQUIRED | `RECONCILE_DISPATCH`, `WAIT` | `RESEND` | `RECONCILE_DISPATCH` | no |
| `INVARIANT_VIOLATION` | MANUAL_REVIEW_REQUIRED | `MANUAL_RESUME` | `RESEND`, `RESUME` | — | yes |
| `MANUAL_CRITERIA` | MANUAL_REVIEW_REQUIRED | `APPROVE_CRITERIA` | — | — | yes |
| `MANUAL_REVIEW` | MANUAL_REVIEW_REQUIRED | `MANUAL_RESUME` | `RESEND` | — | yes |
| `ORPHAN_PRECOMPUTE` | SAFE_TO_RESUME | `DISCARD_SPECULATIVE` | — | `DISCARD_SPECULATIVE` | no |
| `PANIC` | RECONCILIATION_REQUIRED | `MANUAL_RESUME`, `RECONCILE_DISPATCH` | `RESEND` | — | yes |
| `PENDING_CHECKPOINT` | RECONCILIATION_REQUIRED | `RETRY_CHECKPOINT` | — | `RETRY_CHECKPOINT` | no |
| `PROMPT_TOO_LARGE` | MANUAL_REVIEW_REQUIRED | `MANUAL_RESUME` | `RESEND` | — | yes |
| `PROTOCOL_VIOLATION` | MANUAL_REVIEW_REQUIRED | `MANUAL_RESUME` | `RESEND` | — | yes |
| `PROVIDER_UNAVAILABLE` | SAFE_TO_RESUME | `WAIT` | — | `WAIT` | no |
| `REPOSITORY_HEAD_CHANGED` | MANUAL_REVIEW_REQUIRED | `ACCEPT_REPOSITORY_HEAD` | `RESEND` | — | yes |
| `REPOSITORY_IDENTITY_MISMATCH` | MANUAL_REVIEW_REQUIRED | `MANUAL_RESUME` | `RESEND` | — | yes |
| `REPOSITORY_MISSING` | MANUAL_REVIEW_REQUIRED | `MANUAL_RESUME` | `RESEND` | — | yes |
| `REPOSITORY_PERMISSION` | MANUAL_REVIEW_REQUIRED | `MANUAL_RESUME` | — | — | yes |
| `RESPONSE_UNPERSISTED` | RECONCILIATION_REQUIRED | `RECONCILE_DISPATCH`, `RESOLVE_DELIVERY`, `VALIDATE_REPOSITORY_STATE` | `RESEND` | — | yes |
| `RESUMABLE` | SAFE_TO_RESUME | `RESUME` | — | `RESUME` | no |
| `RUNNING_VALIDATION` | SAFE_TO_RETRY | `RUN_VALIDATION` | — | `RUN_VALIDATION` | no |
| `VALIDATION_INFRA` | SAFE_TO_RETRY | `MANUAL_RESUME`, `RUN_VALIDATION` | — | `RUN_VALIDATION` | no |

## Crash matrix

| Operation | Crash location | Outcome | Recovery |
| --- | --- | --- | --- |
| worker dispatch | before intent | CONVERGES_ONCE | the previous step is applied; the dispatch step starts normally |
| worker dispatch | after intent | CONVERGES_ONCE | the INTENT step is resumed; the prompt is prepared and sent once |
| worker dispatch | before external action | CONVERGES_ONCE | the PENDING dispatch was never sent: reconciled as not sent, then sent once |
| worker dispatch | during external action | STOPS_DELIVERY_UNKNOWN | transmitted, outcome unknown: DELIVERY_UNKNOWN, operator decides; never resent |
| worker dispatch | after external action | STOPS_DELIVERY_UNKNOWN | response received but not stored: reconciled through the provider, else DELIVERY_UNKNOWN; never resent |
| worker dispatch | before result persistence | STOPS_DELIVERY_UNKNOWN | same as after external action: the response exists only in the dead process |
| worker dispatch | after result persistence | CONVERGES_ONCE | the stored response is evaluated; nothing is resent |
| worker dispatch | before transition | CONVERGES_ONCE | the step re-runs, finds its completed dispatch and applies the transition |
| worker dispatch | after transition | CONVERGES_ONCE | the transition is durable; the next step starts |
| unit validation | before intent | CONVERGES_ONCE | the previous step is applied; validation starts normally |
| unit validation | after intent | CONVERGES_ONCE | the INTENT step is resumed and starts its run |
| unit validation | before external action | CONVERGES_ONCE | the RUNNING run is ABANDONED and rerun |
| unit validation | during external action | CONVERGES_ONCE | the RUNNING run is ABANDONED (never PASS / FAIL) and rerun |
| unit validation | after external action | CONVERGES_ONCE | results were not recorded: the run is ABANDONED and rerun |
| unit validation | before result persistence | CONVERGES_ONCE | same as after external action |
| unit validation | after result persistence | CONVERGES_ONCE | the recorded result is reused; no rerun |
| unit validation | before transition | CONVERGES_ONCE | the recorded result is reused and the transition applied |
| unit validation | after transition | CONVERGES_ONCE | the transition is durable; the next step starts |
| unit checkpoint | before intent | CONVERGES_ONCE | the previous step is applied; the checkpoint step starts normally |
| unit checkpoint | after intent | CONVERGES_ONCE | the INTENT step is resumed |
| unit checkpoint | before external action | CONVERGES_ONCE | no commit exists (trailer lookup): committed once |
| unit checkpoint | during external action | CONVERGES_ONCE | staged but not committed: no trailer found, committed once |
| unit checkpoint | after external action | CONVERGES_ONCE | the commit exists: adopted by trailer, parent and tree; no second commit |
| unit checkpoint | before result persistence | CONVERGES_ONCE | same as after external action |
| unit checkpoint | after result persistence | CONVERGES_ONCE | the recorded checkpoint is reused |
| unit checkpoint | before transition | CONVERGES_ONCE | the recorded checkpoint is reused and the unit marked DONE |
| unit checkpoint | after transition | CONVERGES_ONCE | the unit is DONE; the next unit is selected |
| workflow transition | before intent | CONVERGES_ONCE | the previous step is applied |
| workflow transition | after intent | CONVERGES_ONCE | the INTENT step is resumed |
| workflow transition | before external action | CONVERGES_ONCE | no external action: as after intent |
| workflow transition | during external action | CONVERGES_ONCE | no external action: as after intent |
| workflow transition | after external action | CONVERGES_ONCE | no external action: as after intent |
| workflow transition | before result persistence | CONVERGES_ONCE | no external action: as after intent |
| workflow transition | after result persistence | CONVERGES_ONCE | no external action: as before transition |
| workflow transition | before transition | CONVERGES_ONCE | the transaction never committed: the step re-runs |
| workflow transition | after transition | CONVERGES_ONCE | the transition is durable |
| phase advancement | before intent | CONVERGES_ONCE | the previous step is applied |
| phase advancement | after intent | CONVERGES_ONCE | the INTENT step is resumed |
| phase advancement | before external action | CONVERGES_ONCE | the phase validation run is ABANDONED and rerun |
| phase advancement | during external action | CONVERGES_ONCE | the phase validation run is ABANDONED and rerun |
| phase advancement | after external action | CONVERGES_ONCE | results not recorded: rerun |
| phase advancement | before result persistence | CONVERGES_ONCE | same as after external action |
| phase advancement | after result persistence | CONVERGES_ONCE | the advancement transaction rolled back: re-applied once; one current phase |
| phase advancement | before transition | CONVERGES_ONCE | as after result persistence |
| phase advancement | after transition | CONVERGES_ONCE | the phase is DONE; the next phase starts |
| project completion | before intent | CONVERGES_ONCE | the previous step is applied |
| project completion | after intent | CONVERGES_ONCE | the INTENT step is resumed |
| project completion | before external action | CONVERGES_ONCE | no final commit exists: committed once |
| project completion | during external action | CONVERGES_ONCE | staged but not committed: committed once |
| project completion | after external action | CONVERGES_ONCE | the final commit exists: adopted by trailer |
| project completion | before result persistence | CONVERGES_ONCE | same as after external action |
| project completion | after result persistence | CONVERGES_ONCE | the completion transaction rolled back: re-applied once |
| project completion | before transition | CONVERGES_ONCE | as after result persistence |
| project completion | after transition | CONVERGES_ONCE | the project is COMPLETED (terminal) |
