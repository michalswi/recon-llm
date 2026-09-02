# recon Application Context

<application_context>

## Purpose

`recon` is a terminal-only Go application that runs an autonomous agent against an Ollama-compatible chat endpoint. It is an orchestrator, not a model runtime. There is no browser UI, HTTP server, user account system, or remote command API.

A user submits an objective. The backend worker repeatedly asks the model for one strict JSON action, executes or records that action, and feeds observations back to the model. A separate critic verifies candidate answers before the terminal asks the user to accept or revise them.

## Repository Map

- `runtime.go`: shared data types, Ollama client, background jobs, statistics, and persistence helpers.
- `agent_terminal.go`: terminal entry point, structured goal input, attachment paths, live event rendering, clarification, approval, revision, and cancellation.
- `agent_worker.go`: run/event model, limits, strict action parser, planning, context compaction, tool execution, critic verification, and cleanup.
- `comm.go`: command-name allowlist.
- `help.go`: terminal help and system-prompt selection.
- `fileextract.go`: local document and packet-capture extraction utilities.
- `vars.go`: version, built-in prompt, environment configuration, and process state.
- `prompts/agent.txt`: strict agent action protocol.
- `prompts/*.txt`: optional base prompts.

## Runtime Configuration

| Variable | Default | Meaning |
|---|---|---|
| `OLLAMA_URL` | `http://localhost:11434/api/chat` | Ollama-compatible chat endpoint |
| `OLLAMA_MODEL` | `qwen3.5:0.8b` | Model used for worker and critic calls |
| `SYSTEM_PROMPT` | unset | Optional path to the initial base prompt |

The application sends non-streaming chat requests. Agent calls set `format: "json"`; thinking is controlled with `/think-on` and `/think-off`.

## Terminal Lifecycle

Plain terminal input starts a run using that text as the objective. `/agent [GOAL]` collects the full `AgentGoalSpec`: objective, expected output, constraints, completion criteria, and optional attachment path.

The selected system prompt is captured as the immutable base prompt for each run and combined with `prompts/agent.txt`. An attachment remains at its existing local path; recon records its metadata and tells the agent where allowed command-line tools can inspect it.

The worker runs synchronously while emitting plans, commands, observations, errors, and verification feedback to the terminal. Ctrl+C cancels the active context and run-owned jobs. The worker pauses in:

- `waiting_input`: the terminal collects clarification and resumes with it as a cumulative requirement.
- `waiting_approval`: the terminal displays the verified candidate and allows accept, revision feedback, or stop.

Terminal run snapshots are persisted under `~/.recon/agent_runs/<run-id>.json`. They provide an audit record; automatic restart/resume is not currently implemented.

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

- The application is agent-only and terminal-only.
- Preserve the worker-owned loop, planning, critic, retries, context compaction, evidence, jobs, attachments, clarification, revision, approval, cancellation, and limits.
- Keep agent actions strict and machine-readable.
- Keep prompt files as runtime filesystem dependencies.
- Keep run snapshots compatible when practical.
- Do not add a Web UI or HTTP command surface without an explicit product change.

</application_context>
