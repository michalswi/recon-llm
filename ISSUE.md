# Pending plan steps can trap successful agent runs in revision loops

## Summary

A run can collect valid evidence, produce a correct candidate answer, and receive approval from the independent critic, but still fail because one or more model-created plan steps remain marked `pending`.

Plan completion currently depends on the worker model emitting a separate, correctly typed `complete_step` action with valid evidence event IDs. Smaller or less protocol-reliable models such as `gemma4:12b` may omit that action, emit the wrong JSON types, or guess event IDs incorrectly. Recon then either repeats the same critic revision until the iteration limit or consumes three protocol-error strikes and fails the run.

This is orchestration bookkeeping failure, not task failure. A model-generated plan should guide execution, but stale plan status should not override an evidence-backed critic approval.

## Observed behavior

Goal:

```text
verify if lamos.space website is up.
```

The worker created two plan steps:

1. Resolve `lamos.space` with DNS.
2. Make an HTTP request and verify that the server responds.

It then successfully executed both checks:

- `dig lamos.space` returned `NOERROR` and two A records.
- `curl -I http://lamos.space` returned `HTTP/1.1 200 OK`.

The worker submitted a candidate answer. The critic response was effectively approved, but recon changed that approval into a rejection because plan step 1 still had status `pending`:

```text
Critic requested revision: plan step 1 is not completed: Perform a DNS lookup for lamos.space to check if the domain resolves.
```

The same revision repeated. The worker eventually attempted to repair the plan state, but produced three invalid actions:

```json
{"action":"complete_step","stepId":1,"evidenceEventIds":["2"]}
```

This failed because `evidenceEventIds` requires integers.

```json
{"action":"complete_step","stepId":1,"evidenceEventIds":[2]}
```

This failed because event 2 was the `create_plan` action, not evidence.

```json
{"action":"complete_step","stepId":1,"evidenceEventIds":[1]}
```

This failed because event 1 was a status event, not evidence.

After three protocol/state validation failures, recon terminated the run even though the requested DNS and HTTP checks had succeeded.

A separate run showed the more expensive form of the same bug: the worker repeatedly submitted an answer without ever attempting `complete_step`. Each candidate invoked the critic, was rejected by recon's pending-plan check, and repeated until the 30-iteration limit.

## Expected behavior

When the independent critic approves a candidate and cites valid evidence that satisfies the objective, constraints, completion criteria, and required-command checks, recon should present the candidate for user approval even if a model-created plan still contains pending steps.

Plan state should remain useful for progress reporting and evidence organization, but it should not be a second completion contract that can contradict the critic and the observed evidence.

## Current control flow

The relevant behavior is in `agent_worker.go`.

### Plan creation and completion

`executeAgentAction` creates each plan step with status `pending` when it processes `create_plan`.

A step changes to `completed` only when the model later emits `complete_step`. Recon validates every cited event through `validateAgentActionState` and `agentEvidenceEventIsValid` before mutating the plan.

Successful `run_command`, `list_directory`, `search_files`, or terminal job observations do not automatically complete a plan step.

### Candidate verification

When the worker emits `answer`, `runAgent` calls `verifyAgentAnswer`.

The critic receives the goal, revision context, plan, candidate, and recorded evidence. After decoding the critic response, recon applies this additional deterministic rule:

```go
for _, step := range run.Plan {
    if verification.Approved && step.Status != "completed" {
        verification.Approved = false
        verification.Feedback = fmt.Sprintf(
            "plan step %d is not completed: %s",
            step.ID,
            step.Description,
        )
    }
}
```

This rule runs after the critic has evaluated the answer. Therefore, the repeated message in the logs indicates that the critic approved the candidate and recon locally downgraded it solely because of stale plan bookkeeping.

### Failure accounting

Critic rejections use `recordAgentRevision`, which resets `ConsecutiveErrors`. An identical pending-step rejection can therefore repeat until the iteration limit.

Malformed action JSON and invalid `complete_step` evidence use `recordAgentFailure`. Three consecutive mistakes terminate the run, even when valid task evidence already exists.

## Root cause

Recon assigns two incompatible roles to the plan:

- The prompt describes it as a model-managed work plan.
- The verifier treats every step's explicit completion flag as a mandatory correctness invariant.

The completion flag is not derived from execution. It depends on the same worker model remembering to perform bookkeeping, formatting the action exactly, and selecting correct event IDs. This makes task success depend on protocol behavior unrelated to whether the objective was actually completed.

Prompt changes alone cannot make this reliable. They may improve compliance, but they cannot guarantee that every supported model will emit valid bookkeeping actions.

## Recommended fix

### Make plan completion advisory during answer verification

Remove the deterministic loop in `verifyAgentAnswer` that changes an approved verification to rejected solely because a plan step is pending.

The final decision should continue to depend on:

- The critic's assessment of the objective, expected output, constraints, completion criteria, plan, candidate, and evidence.
- Validation that every critic-cited event exists.
- Validation that cited events are acceptable evidence.
- Existing explicit command requirements.
- Existing code-deliverable requirements.

The critic already receives the plan and can reject a candidate when a substantive planned task was not performed. Recon should enforce evidence validity, but it should not equate an unset bookkeeping flag with missing evidence.

Pending steps may remain visible in the persisted run for diagnostics. They should not prevent transition to `waiting_approval` after an otherwise valid critic approval.

### Preserve strict `complete_step` validation

Do not weaken evidence validation merely to accommodate guessed IDs. If the model chooses to emit `complete_step`, recon should continue requiring:

- A positive existing step ID.
- Integer evidence event IDs.
- Existing events.
- Evidence event kinds that are valid under `agentEvidenceEventIsValid`.

Accepting arbitrary numeric strings or invalid event references would hide model mistakes and weaken the evidence ledger. Removing the hard answer gate makes such permissiveness unnecessary for successful task completion.

## Optional hardening

These changes are useful but are not required for the minimum fix.

### Expose valid evidence IDs in recovery feedback

When `complete_step` validation fails, include a concise list of currently valid evidence events in the correction, for example:

```text
Valid evidence events: 4 run_command dig lamos.space; 6 run_command curl -I http://lamos.space.
```

This helps models recover without guessing. Output should be bounded to avoid prompt growth.

### Treat plan-bookkeeping mistakes as recoverable revisions

A syntactically valid `complete_step` action with an invalid step or evidence reference could use `recordAgentRevision` instead of `recordAgentFailure`. The task evidence is not invalidated by a bookkeeping mistake.

Malformed top-level JSON should retain the existing protocol-failure policy unless a narrow and testable parser classification is introduced. Recon should not add loose JSON coercion globally.

### Detect identical revision loops

Track repeated identical verification feedback. After a small threshold, stop repeating the same expensive critic cycle and report a targeted failure or request user input.

This is defense in depth. It should not replace fixing the incorrect pending-plan gate.

### Improve prompt wording

The protocol prompt can state that `complete_step` is progress bookkeeping and must use the numeric event sequence shown in `RECENT RUN EVENTS`. Prompt changes improve model behavior but are not sufficient as the primary fix.

## Alternatives considered

### Block `answer` before invoking the critic when steps are pending

This avoids expensive critic calls but still makes successful completion depend on model bookkeeping. A model that ignores or mishandles `complete_step` remains trapped. This is not a complete fix.

### Automatically match observations to plan steps

Recon could infer that `dig` satisfies a DNS step and `curl` satisfies an HTTP step. Reliable matching would require semantic interpretation and can associate evidence with the wrong step. Simple keyword matching is too fragile to become a correctness boundary.

### Complete all pending steps from critic evidence

Recon could mark pending steps complete after critic approval. However, the critic returns evidence for the candidate as a whole, not a mapping from each event to each plan step. Assigning the same evidence to every pending step would overstate what each event proves.

### Remove planning entirely

Planning remains useful for multi-step execution, progress display, compaction, and evidence organization. Only its role as an unconditional final-answer gate needs to change.

## Proposed implementation scope

1. Remove the pending-plan downgrade loop from `verifyAgentAnswer`.
2. Keep the plan in the critic prompt so unfinished substantive work can still cause critic rejection.
3. Keep all critic evidence validation and required-goal checks unchanged.
4. Add regression tests for approved candidates with pending plans.
5. Optionally add bounded valid-evidence hints and repeated-revision protection as separate changes.
6. Update `APP.md` and `README.md` to clarify that plans guide execution and completed steps require valid evidence, but pending step status alone does not override critic approval.

## Required tests

### Approved candidate with pending plan

Create a run with:

- One or more pending plan steps.
- Successful command observations.
- A critic response with `approved: true` and valid evidence IDs.

Assert that `verifyAgentAnswer` remains approved.

### Pending plan remains visible

After approval, assert that the plan is not silently rewritten or assigned evidence. Its pending status should remain available for diagnostics unless a separate explicit completion mechanism changes it.

### Critic rejection still rejects

Return `approved: false` with substantive feedback while plan steps are pending. Assert that the rejection and feedback are preserved.

### Invalid critic evidence still downgrades approval

Return `approved: true` with a nonexistent event ID or an event that cannot prove the answer. Assert that existing evidence checks still reject it.

### Required command enforcement remains active

For a goal that explicitly requires a command, return critic approval without valid evidence for that command. Assert that recon still rejects the candidate.

### Valid explicit step completion still works

Keep the existing tests proving that `complete_step` accepts valid evidence and rejects failed or non-evidence events.

### End-to-end regression

Simulate this sequence:

1. `create_plan` with DNS and HTTP steps.
2. Successful `dig` observation.
3. Successful `curl` observation.
4. `answer` without any `complete_step` actions.
5. Critic approval citing the successful observations.

Assert that the run reaches `waiting_approval` rather than emitting `plan step ... is not completed`, exhausting iterations, or failing from bookkeeping strikes.

## Acceptance criteria

- A critic-approved, evidence-backed answer is not rejected solely because a model-created plan step is pending.
- Missing substantive work can still be rejected by the critic.
- Invalid, failed, or nonexistent evidence cannot support approval.
- Explicit command and code-deliverable safeguards remain unchanged.
- Strict `complete_step` validation remains intact.
- The reproduced DNS/HTTP scenario reaches `waiting_approval` without requiring the worker to emit `complete_step`.
- Existing tests and `go test -race ./...` pass.

## Risk assessment

The primary behavioral change is that plan status becomes advisory at final verification. This is appropriate because plans are generated by the worker, not supplied as immutable user requirements. The critic still sees the plan and all validated evidence, while deterministic checks continue protecting explicit user requirements and evidence integrity.

The main residual risk is an overly permissive critic approving an answer despite an actually unfinished planned task. That risk already exists for goals without plans and should be addressed through critic quality and deterministic checks tied to explicit user requirements, not through model-managed bookkeeping flags.
