# Agent Loop Review

This document reviews how the `recon` agent loop actually works today and lists concrete
improvement ideas. Nothing here has been implemented yet — it is a review artifact so we can
decide together which ideas to act on. **No existing capability should be removed without an
explicit decision**, per the product invariants in [APP.md](APP.md).

## How it works today

One iteration of `runAgent` ([agent_worker.go](agent_worker.go)):

1. Check status/iteration/deadline; stop if the run is no longer `running`.
2. `compactAgentContext` — if pending (uncompacted) events exceed the 4096-token budget, ask the
   model to summarize the oldest ones into `ContextSummary` and advance `CompactedThrough`.
3. Build `messages` via `agentMessages`: base prompt + `prompts/agent.txt` protocol + execution
   environment (OS, cwd, available/unavailable commands) + revision context + current plan +
   summary + a token-budgeted window of recent events replayed as chat turns + a final instruction.
4. Call the model (`callOllamaMessageWithLogContext`, `format: "json"`, 2-minute deadline).
5. Parse strictly as one `ToolResponse` JSON object (`parseAgentAction`); reject unknown fields
   and trailing content.
6. `validateAgentActionState` — cross-check job ownership, plan step/evidence IDs exist.
7. Force recovery: if the last command observation failed and this isn't a 2nd consecutive
   failure, reject anything but `run_command`. If the goal textually requires a specific
   allowlisted command and it hasn't produced a successful observation yet, block `answer`.
8. If the action is `answer`: check a code-deliverable heuristic, check the required-command
   heuristic, then send the candidate to an **independent critic** model call
   (`verifyAgentAnswer`) with its own JSON schema, plan-completion check, and evidence-ID
   validation. Rejection becomes revision context; approval hands control to the terminal/API for
   user accept/revise/stop.
9. Append the selected action and persist it, execute the tool (`executeAgentAction`) — shell
  command, background job, job inspection, native `list_directory`/`search_files`, plan mutation,
  findings — append its resulting event, then persist again to `~/.recon/agent_runs/<id>.json`.
10. Repeat, up to 30 iterations / 30 minutes, with a 3-consecutive-failure kill switch.

Command execution goes through an allowlist (`comm.go`) checked per `|`/`;`/`&`-separated segment
in `validateAgentCommand`, blocking `$()`/backticks, multi-line input, and path-qualified
executable names.

## Strengths worth preserving

- Strict single-JSON-object protocol with unknown-field rejection — makes parsing deterministic
  and testable.
- Independent critic step before any answer reaches the user, plus plan-completion and
  evidence-ID cross-checks — a real "second opinion" rather than the same call trusting itself.
- Deterministic native `list_directory`/`search_files` remove a whole class of shell-quoting
  failures the model would otherwise retry into the failure budget.
- Forced recovery nudges (failed command, required command, invalid action) reduce wasted
  iterations without hardcoding tool names into prompts (per existing prompt conventions).
- Context compaction preserves a running summary instead of hard-truncating, so long runs
  degrade gracefully.
- Evidence-ID citation requirement makes "the agent claims it did X" checkable against real
  events instead of trusted prose.

## Improvement ideas

Ordered by impact. The default rule for every item is: preserve existing successful workflows,
add characterization tests before changing behavior, and keep compatibility-sensitive policy
changes separate from reliability fixes.

### 1. Compaction can hide evidence from the critic (additive correctness fix) — IMPLEMENTED
`compactAgentContext` replaces older events with `ContextSummary`, but `verifyAgentAnswer` sends the
critic only the recent uncompacted events. It does not send `ContextSummary`, and it does not pull
in events cited by completed plan steps. A long run can therefore complete valid work, compact its
supporting observations, and then reach a critic that cannot inspect the evidence. Depending on
the model, this can cause either a false rejection or an approval based on an unsupported summary
in the candidate.

There is also a fallback data-loss case: when the summary model call fails,
`truncateAgentText(run.ContextSummary + "\n" + summaryInput)` retains the oldest prefix. Once that
prefix reaches 32 KiB, newly compacted events can be discarded even though `CompactedThrough`
still advances past them.

Preserving fix: include `ContextSummary` in critic input and retain a bounded evidence ledger that
keeps directly cited observations available by event ID after compaction. Keep the existing
recent-event budget for conversational history. Make fallback compaction retain the newest facts
before advancing `CompactedThrough`. Add tests that force successful and failed compaction, cite an
older observation, and verify that the critic receives both historical context and direct evidence.

Implemented in [agent_worker.go](agent_worker.go): `verifyAgentAnswer` now inlines
`run.ContextSummary` into the critic's user message (when non-empty) and appends
`agentEvidenceLedger` — the events cited by completed plan steps (`agentPlanEvidenceEventIDs`),
looked up directly from `run.Events` by ID and deduped against the recent-context window — to the
evidence sent to the critic. `compactAgentContext`'s fallback path now calls
`mergeAgentSummaryFallback`, which keeps the newly compacted text intact and truncates from the
front of the older summary instead of the back, so `CompactedThrough` never advances past facts
that were silently dropped. Covered by new tests in
[agent_worker_test.go](agent_worker_test.go).

### 2. Background-job state has unsynchronized pointer access (additive correctness fix) — IMPLEMENTED
`JobStore.get` and `listBySession` return pointers after releasing the store lock. The background
goroutine, `check_job`, `cancel_job`, cleanup, and `run_command_bg` then read or mutate fields on
the same `BackgroundJob` concurrently. `run_command_bg` also assigns `RunID` after starting the
goroutine. This can produce data races and makes cleanup timing-dependent.

Preserving fix: assign `RunID` before process execution, keep all mutable job transitions behind
`JobStore` methods, and return immutable snapshots rather than internal pointers. Preserve the
existing action schemas, job IDs, status strings, output behavior, one-hour timeout, and cleanup
semantics. Ensure cancellation remains `cancelled` after `cmd.Run` returns instead of being
overwritten as `failed`, and terminate the owned process tree rather than only the shell when this
can be implemented portably. Add lifecycle and cancellation tests and run them with
`go test -race ./...`.

The same snapshot discipline should apply to `AgentRun`: `agentSnapshot` currently leaves `Plan`
sharing its backing array, and `cloneAgentRun` copies plan steps without copying each step's
`EvidenceEventIDs`. Deep-copy both levels so API encoding, prompt construction, and persistence
cannot observe concurrent plan mutation.

Implemented: `BackgroundJob` ([runtime.go](runtime.go)) is now an immutable value snapshot with no
`Cmd` field; `JobStore` stores mutable state (plus the live `*exec.Cmd`) in an unexported
`jobRecord` behind its mutex and exposes only `get`/`create`/`attachCmd`/`listBySession`/`finish`/
`cancel`/`cancelRun`, all returning value copies. `executeBackgroundCommand` now takes `runID` and
assigns it at creation (before the process starts), and calls `cmd.Start()` synchronously so
`cmd.Process` is fully initialized before the job is attached and visible to `cancel`/`cancelRun` —
removing the prior unsynchronized read of `cmd.Process`. `finish`/`cancel` only apply a transition
when the job is still `"running"`, so a process that dies from a `cancel`-triggered kill can never
overwrite the `"cancelled"` status back to `"failed"`. Cancellation now kills the job's whole
process group (`killProcessGroup`, POSIX process groups via `configureProcessGroup`/`Setpgid` on
darwin/linux in [procgroup_unix.go](procgroup_unix.go), falling back to killing only the top-level
process on other platforms in [procgroup_other.go](procgroup_other.go)) instead of only the `bash`
shell. `agentSnapshot` ([agent_worker.go](agent_worker.go)) and `cloneAgentRun` ([runtime.go](runtime.go))
both now deep-copy `Plan` via a shared `clonePlanSteps` helper that also copies each step's
`EvidenceEventIDs`. Covered by new tests in [runtime_test.go](runtime_test.go) (lifecycle,
cancel-not-overwritten, wrong-session cancel rejection, session-scoped listing, concurrent-access
`-race` check, plan/snapshot deep-copy isolation). Full suite + `-race` pass; cross-compiles for
linux/amd64 and windows/amd64 verified.

### 3. Evidence IDs prove existence, not validity (additive correctness fix) — IMPLEMENTED
`validateAgentActionState` and `verifyAgentAnswer` reject out-of-range evidence IDs, but most IDs
are not checked for evidence type, successful outcome, or relationship to the claim. A completed
step or critic can therefore cite a failed command, an invalid action, or an unrelated event. The
special required-command path is stronger because `evidenceSupportsCommand` checks for a
successful matching command, but that protection does not apply generally.

Preserving fix: define an explicit evidence policy per action instead of globally requiring
`Success == true`. A successful foreground/native observation can prove its result; a background
start can prove only that a job started; a completed job snapshot can prove its final result; user
feedback can prove a requirement but not command execution. Validate deterministic properties in
code and leave semantic relevance to the critic. This preserves legitimate non-command evidence
while rejecting structurally impossible proof. Add negative tests for failed, error, candidate,
and unrelated lifecycle events, plus positive tests for every supported evidence-producing action.

Implemented in [agent_worker.go](agent_worker.go): new `agentEvidenceEventIsValid` applies a
deterministic per-action policy — `run_command`/`list_directory`/`search_files` observations must
have `Success == true`; `run_command_bg` observations are always valid (they only ever claim a job
started); `check_job`/`get_job` observations are valid only when the embedded job status (parsed
via `agentJobObservationStatus`) is not `"running"` (a terminal snapshot proves a final result, an
in-progress one proves nothing yet); `cancel_job`/`list_jobs`/`update_findings` observations,
`feedback`, and `plan` events are always valid; every other kind (`action`, `invalid_action`,
`candidate`, `answer`, `verification`, `clarification`, `status`, `error`) is never valid evidence.
`validateAgentActionState`'s `complete_step` check and `verifyAgentAnswer`'s post-approval evidence
check both now call this policy — an invalid citation in `complete_step` is rejected the same way
as a nonexistent one, and an invalid citation from the critic downgrades approval to a revision
(clear feedback) rather than an error, consistent with the existing required-command rejection
path. Covered by new tests in [agent_worker_test.go](agent_worker_test.go) (table test over every
event kind/action combination, `complete_step` accept/reject, and an httptest-based critic
downgrade test). Full suite + `-race` pass.

### 4. Model-call failures always consume the 3-strike budget (additive availability fix) — IMPLEMENTED
`recordAgentFailure` treats a transient network blip, a request timeout, an Ollama 5xx response,
and a genuinely invalid model response identically. A flaky endpoint can burn all 3 strikes even
though the agent's reasoning never ran.

Preserving fix: retry only clearly transient transport failures and HTTP 429/5xx responses with a
small capped backoff before incrementing `ConsecutiveErrors`. Keep parse, schema, state-validation,
and deterministic 4xx failures on the existing immediate failure path. Respect the enclosing
30-minute run deadline and cancellation on every retry; do not retry after cancellation. Tests
should use an `httptest` server, not timing-sensitive access to a real Ollama instance.

Implemented in [runtime.go](runtime.go): non-2xx Ollama responses now return a typed
`ollamaHTTPStatusError{StatusCode, Status}` instead of a bare formatted error. New
`isRetryableOllamaTransportError` classifies only HTTP 429/5xx responses and `*url.Error` (network
dial failures, TLS errors, and the per-attempt request timeout) as retryable; any other 4xx or an
undecodable response body is left on the immediate-failure path, unchanged. New
`callOllamaMessageWithRetry(ctx, ...)` wraps `callOllamaMessageWithLogContext` with up to 3 attempts
and a capped exponential backoff (`ollamaRetryBaseBackoff`, doubling each attempt): it takes the
caller's own outer context directly (not a pre-wrapped one) and derives a fresh per-attempt
`agentModelTimeout` deadline from it on every attempt, so a single slow attempt doesn't shrink the
budget available to a retry; it never retries once the outer context is done (run deadline or
cancellation), and the backoff sleep itself is interrupted by that same context. All three model
call sites in [agent_worker.go](agent_worker.go) — the main iteration loop, `verifyAgentAnswer`,
and `compactAgentContext`'s fallback summary call — now go through this wrapper instead of calling
`callOllamaMessageWithLogContext` directly, so a flaky endpoint no longer burns a strike (or forces
a premature compaction fallback) before the agent's own reasoning ever ran. Covered by new tests in
[runtime_test.go](runtime_test.go) using `httptest` servers (no real Ollama, no fixed sleeps other
than a test-overridable `ollamaRetryBaseBackoff`): retry-classification table test, succeed-after-
one-transient-failure, no-retry-on-deterministic-4xx, exhausts-all-3-attempts-then-fails, and
cancellation-interrupts-backoff. Full suite + `-race` pass.

### 5. `sh` and `xargs` sit in the command allowlist and can bypass it entirely (behavior change — needs a decision)
`comm.go` allows `sh`. `validateAgentCommand` only inspects the first word of each
`|`/`;`/`&`-separated segment, so `sh -c "<anything>"` passes validation and then `bash -c` runs
whatever `sh -c` was given, completely unchecked against the allowlist. `xargs <command>` has the
same property: `xargs` itself is allowlisted, but the command it invokes is not re-validated.
This means the allowlist is not actually a boundary today whenever the model (correctly or via a
bad response) reaches for `sh` or `xargs`.
- If `sh`/`xargs` usage is intentional (e.g., needed for legitimate multi-command workflows),
  keep them but say so explicitly in README's security section instead of implying the allowlist
  is comprehensive.
- If not needed, removing them closes the gap without touching the rest of the protocol. This is
  a functionality change (some goals that currently rely on `sh -c` chains would need an
  equivalent allowlisted command instead), so it should be an explicit choice, not a silent edit.

There is a second problem in the same validator: splitting on punctuation is not shell-aware, so
separators inside quoted arguments can be misread as new commands. If command policy is revisited,
use a real shell parser and characterization tests for every currently supported form rather than
adding more string splitting. Do not remove `sh`, `xargs`, quoting, pipelines, redirections, or
other accepted forms as part of an unrelated reliability change.

### 6. Goal-requirement detection is keyword heuristics on free text (instrument first)
`requiredGoalCommand` and `goalRequiresCodeDeliverable` scan the objective/expected-output/
completion-criteria text for bare words like `run`, `execute`, or `write`/`build`+`code`/`app`.
This can both over-trigger (a goal that merely *mentions* a command name in passing) and
under-trigger (phrasing that doesn't match the exact verb list). It works today because it only
adds an extra requirement on top of the critic, never silently approves something it shouldn't,
so the failure mode is "unnecessary extra command" rather than "bad answer accepted." Given that
safety margin, this is lower priority than items 1-2, but worth revisiting if false positives are
observed in practice (e.g., log how often `requiredGoalCommand` fires vs. how often the critic
would have caught the same case anyway).

### 7. API run registry grows without bound (additive, operational)
`agentAPI.runs` (in [agent_api.go](agent_api.go)) is a process-local map that never evicts
entries — every run created over the process lifetime stays in memory (events, messages,
snapshots) even long after `completed`/`failed`/`cancelled`. For a long-lived API process handling
many runs this is a slow memory leak. Idea: add a bounded LRU-style eviction (or a TTL sweep) for
runs that reached a terminal status more than N minutes ago, keeping the on-disk snapshot as the
audit record (already the documented behavior — the API doesn't reload snapshots after restart
anyway). Purely additive; doesn't change any request/response shape.

### 8. Action schemas are syntactically strict but not action-specific (additive hardening)
Unknown JSON fields are rejected, but `ToolResponse` is one shared struct, so fields belonging to
another known action are accepted and silently ignored. For example, an `answer` can carry a
`command`, or `list_jobs` can carry `text`. This is not currently an execution vulnerability, but
it weakens the protocol's claim that each action follows exactly one schema and can conceal model
confusion.

Preserving fix: validate that fields not belonging to the selected action have zero values. Start
with telemetry or tests against real model outputs; rejecting newly detected malformed actions
can change recovery behavior, so land this independently from the correctness fixes above.

### 9. Persistence is frequent but intentionally crash-oriented (minor, measure first)
The loop persists before executing an action and again after execution, plus at terminal state
transitions. This makes disk I/O scale with iterations but prevents an unrecorded side effect when
the process exits between model selection and execution. Do not batch or remove these writes
without a crash-consistency test proving that command intent and resulting evidence remain
auditable. Optimize only if profiling shows this is material.

### 10. No coordination between multiple simultaneously running agent instances [toreview]
Today, "many instances" only works in the sense that separate `recon` processes (or separate runs
within one API process) don't share in-memory state and therefore can't corrupt each other's
worker loop, job store, or run registry — confirmed safe after the #1/#2 fixes above. But there is
no actual cooperation model: each instance/run is fully independent and unaware of any other.

Concretely, running several instances (or agents) at once today means:
- No shared work queue, locking, or leader election — two instances can be pointed at the same
  target/goal and duplicate all work with no visibility into each other.
- No cross-instance handoff of findings, plans, or evidence; `ContextSummary`/plan/evidence all
  live only inside one run's `AgentRun`, so one agent cannot delegate a sub-task to another or
  merge results.
- Shared filesystem resources are not coordinated: `~/.recon/history` (terminal only) is appended
  without any lock, and nothing prevents two instances from being pointed at the same attachment
  path or output location concurrently.
- `agentAPI.runs` (see #7) and `jobStore` are process-local, so a load balancer or process
  supervisor cannot see or manage runs/jobs started in a sibling process — there is no shared
  registry an orchestrator could query across instances.
- Command execution has no per-target concurrency control, so nothing stops two instances from
  running conflicting or redundant commands (e.g. two `nmap` scans, or two writers to the same
  file) against the same host at the same time.

This item is flagged for review only — it changes the product's scope (single-agent orchestrator
today vs. a coordinated multi-agent system) rather than fixing a bug, so it needs an explicit
decision on the desired cooperation model (e.g. shared run registry service, message-passing
between agents, target-level locking, or simply documenting that multi-instance use requires
external coordination) before any implementation is proposed.

## Test gaps to close first

Current tests cover the API clarification/approval lifecycle, attachment propagation, native
directory/search tools, parser basics for those native actions, and terminal line editing. Before
changing the loop, add focused tests for:

- command validation across every allowlisted shell form currently accepted, including quoting,
  pipelines, redirections, `sh -c`, and `xargs`;
- critic input after compaction and evidence references to compacted events;
- fallback compaction preserving newly summarized events when the model call fails;
- transport retry classification, cancellation, and deadline exhaustion;
- background-job start/check/get/cancel/cleanup under `go test -race`;
- deep-copy isolation for plans and nested evidence-ID slices;
- admissible evidence types and outcomes for plan completion and critic approval;
- plan evidence validation and critic rejection when evidence is missing or unrelated;
- three-strike behavior for parse and state-validation failures.

## Recommendation

Use this sequence to improve reliability without losing functionality:

1. Add the characterization and race tests above without changing production behavior.
2. Fix compacted critic evidence (#1), shared-state synchronization (#2), and evidence validity
  (#3); these repair existing behavior without removing an action or workflow.
3. Add narrowly classified transport retries (#4), preserving cancellation and global deadlines.
4. Bound completed API runs (#7), with documented retention and tests that active/waiting runs are
  never evicted.
5. Instrument the heuristics (#6) before changing them and harden action fields (#8) separately.
6. Decide command-policy goals explicitly before touching `sh`, `xargs`, or shell parsing (#5).
  Security tightening and compatibility preservation cannot be assumed to be the same change.

For each production change, keep the JSON protocol, terminal/API states, persisted snapshot shape,
attachments, planning, evidence IDs, critic gate, background jobs, cancellation, and all currently
successful command workflows intact. Run `go test ./...` and `go test -race ./...` at each step.
