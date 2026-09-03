# recon Application Context

<application_context>

## Purpose

`recon` is a Go application that runs an autonomous agent against an Ollama-compatible chat endpoint. It is an orchestrator, not a model runtime. It has a terminal interface and an opt-in headless HTTP API for integration testing and automation. There is no browser UI or user account system.

A user submits an objective. The backend worker repeatedly asks the model for one strict JSON action, executes or records that action, and feeds observations back to the model. A separate critic verifies candidate answers before the terminal asks the user to accept or revise them.

## Repository Map

- `runtime.go`: shared data types, Ollama client, Ollama reachability check, background jobs, statistics, and persistence helpers.
- `agent_terminal.go`: terminal entry point, structured goal input, attachment paths, live event rendering, clarification, approval, revision, and cancellation.
- `agent_api.go`: opt-in HTTP entry point for creating, inspecting, resuming, approving, and cancelling agent runs.
- `agent_worker.go`: run/event model, limits, strict action parser, planning, context compaction, tool execution, critic verification, and cleanup.
- `comm.go`: command-name allowlist.
- `help.go`: terminal help and system-prompt selection.
- `fileextract.go`: local document and packet-capture extraction utilities.
- `vars.go`: version, built-in prompt, environment configuration, and process state.
- `prompts/agent.txt`: strict agent action protocol embedded at build time.
- `prompts/*.txt`: base prompts embedded at build time.

## Runtime Configuration

| Variable | Default | Meaning |
|---|---|---|
| `OLLAMA_URL` | `http://localhost:11434/api/chat` | Ollama-compatible chat endpoint |
| `OLLAMA_MODEL` | `qwen3.5:0.8b` | Model used for worker and critic calls |
| `SYSTEM_PROMPT` | unset | Optional path to the initial base prompt |
| `RECON_API_ADDRESS` | unset | Run the HTTP API at this address instead of the terminal |

The application sends non-streaming chat requests. Agent calls set `format: "json"`; thinking is controlled with `/think-on` and `/think-off`.

## Diagnostics

`-check-ollama` queries the configured `OLLAMA_URL` host's `/api/tags` endpoint, prints reachability and model count, and exits without starting the terminal or API. The terminal `/ollama` command runs the same check interactively during a session.

`-check-ollama-ps` queries the host's `/api/ps` endpoint, prints currently loaded models with size and expiry, and exits the same way. The terminal `/ollama-ps` command runs the same check interactively.

## Terminal Lifecycle

Plain terminal input starts a run using that text as the objective. `/agent [GOAL]` collects the full `AgentGoalSpec`: objective, expected output, constraints, completion criteria, and optional attachment path.

The selected system prompt is captured as the immutable base prompt for each run and combined with the embedded agent protocol. An attachment remains at its existing local path. Supported image attachments are base64-encoded into Ollama messages for worker and critic vision analysis; other files are exposed by path for allowed command-line tools. A plain objective can infer an existing absolute image path, while `/agent` and the API accept explicit attachment paths. `/attach` persists a file or directory's regular files across subsequent terminal runs until `/deattach` or `/detach`; `/attached` lists the current set. At most 16 files are attached per run.

The worker runs synchronously while emitting plans, commands, observations, errors, and verification feedback to the terminal. Ctrl+C cancels the active context and run-owned jobs. The worker pauses in:

- `waiting_input`: the terminal collects clarification and resumes with it as a cumulative requirement.
- `waiting_approval`: the terminal displays the verified candidate and allows accept, revision feedback, or stop.

Terminal run snapshots are persisted under `~/.recon/agent_runs/<run-id>.json`. They provide an audit record; automatic restart/resume is not currently implemented.

## API Lifecycle

The API is enabled with `-api-address <address>` or `RECON_API_ADDRESS`; it is disabled by default and replaces the interactive terminal loop for that process. It reuses the terminal session constructor and worker rather than implementing a separate agent engine.

- `POST /api/agent/runs` creates and asynchronously starts a run from an `AgentRequest` JSON object.
- `GET /api/agent/runs/{id}` returns sanitized run state, events, plans, and candidate output.
- `POST /api/agent/runs/{id}/feedback` resumes clarification or revises a candidate.
- `POST /api/agent/runs/{id}/approve` accepts a verified candidate.
- `POST /api/agent/runs/{id}/cancel` cancels active work and run-owned jobs.

The in-memory API registry is process-local. Persisted snapshots remain audit records and are not loaded after restart. The API has no authentication and must remain loopback-bound or be protected by a trusted authenticated proxy.

## Agent Actions

Supported actions are:

- `create_plan`
- `complete_step`
- `run_command`
- `run_command_bg`
- `check_job`
- `get_job`
- `cancel_job`
- `list_jobs`
- `update_findings`
- `request_clarification`
- `answer`

The decoder requires exactly one JSON object and rejects unknown fields or trailing content. Tool failures become observations so the agent can recover.

## Limits and Verification

- 30 iterations per run segment and a 30-minute segment deadline.
- Two-minute model and foreground-command deadlines.
- 4096-token recent-event context plus a 768-token compaction instruction budget.
- 32 KiB maximum observation size in model context.
- Three consecutive protocol or model failures.
- Explicit code goals require a non-empty fenced code block.
- Explicit command goals require a real matching command observation.
- Completed plan steps require valid evidence event references.
- Candidate answers require independent critic approval before user approval.
- Critic rejection and user feedback remain in revision context after event compaction.

## Command and Trust Boundary

The nominal command allowlist is defined in `comm.go`. Agent commands are validated before execution, but execution is not sandboxed and uses the operating-system permissions of the recon process. Background commands use `bash -c`; run cleanup cancels jobs owned by that run.

Do not claim the allowlist is a security boundary. Use recon only in a trusted environment and only for authorized targets.

## Product Invariants

- The application is agent-only, with terminal and opt-in headless API interfaces.
- Preserve the worker-owned loop, planning, critic, retries, context compaction, evidence, jobs, attachments, clarification, revision, approval, cancellation, and limits.
- Keep agent actions strict and machine-readable.
- Keep bundled prompts embedded so the binary remains standalone.
- Keep run snapshots compatible when practical.
- Do not add a Web UI without an explicit product change.

</application_context>
