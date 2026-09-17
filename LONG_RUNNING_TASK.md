# Long-Running Task Behavior

How recon behaves when given a long-running task, based on the current implementation in `agent_worker.go` and `agent_api.go`.

- **Iteration cap**: 30 iterations per run (`agentMaxIterations`). Hitting it ends the run with status `limit_reached`.
- **Wall-clock cap**: a 30-minute run deadline (`agentRunTimeout`, applied via `agentRunContext`). If it fires first, the run also ends as `limit_reached`.
- **Per-step cap**: each model call and each foreground `run_command` has its own independent 2-minute deadline (`agentCommandTimeout`) — a single slow command won't hang the run, it just fails that step as an observation.
- **For genuinely long external work**: use `run_command_bg` to start a background job, then poll with `check_job`/`get_job` across iterations. This is the intended way to survive work that exceeds the 2-minute foreground timeout without burning the whole 30-minute budget on one blocked call.
- **Context growth**: a 4096-token recent-event window plus compaction keeps the prompt bounded across many iterations, so a long task degrades gracefully rather than blowing up context — but this doesn't extend the iteration/time budget itself.
- **`limit_reached` is terminal**: unlike `waiting_approval`/clarification states, it cannot be resumed via the feedback endpoint (that requires the run to already be waiting on the user). A new run must be started.

Summary: long tasks work fine as long as the model offloads slow work to background jobs, but recon hard-stops and reports `limit_reached` at 30 iterations or 30 minutes, whichever comes first, with no auto-resume.
