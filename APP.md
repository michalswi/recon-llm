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
- `procgroup_unix.go`/`procgroup_other.go`: background-job process-group start/kill, platform-specific.
- `vars.go`: version, built-in prompt, environment configuration, and process state.
- `prompts/agent.txt`: strict agent action protocol embedded at build time.
- `prompts/*.txt`: base prompts embedded at build time.

## Runtime Configuration

| Variable | Default | Meaning |
|---|---|---|
| `OLLAMA_URL` | `http://localhost:11434/api/chat` | Ollama-compatible chat endpoint |
| `OLLAMA_MODEL` | `qwen3.5:0.8b` | Model used for worker and critic calls |
| `OLLAMA_CA_CERT` | unset | Optional path to a PEM CA certificate trusted in addition to the system pool, for a self-signed cert on a TLS-terminating Ollama reverse proxy; has no effect unless `OLLAMA_URL` uses `https://` |
| `OLLAMA_INSECURE_SKIP_VERIFY` | `false` | Disables TLS certificate validation for Ollama connections and ignores `OLLAMA_CA_CERT`. Testing only — a network attacker can impersonate the endpoint undetected |
| `OLLAMA_NUM_PREDICT` | `-1` (no limit) | Sent as `options.num_predict` on every chat request, overriding any smaller default baked into a model's Modelfile so long answers (e.g. generated code) aren't truncated mid-response |
| `OLLAMA_NUM_CTX` | unset | Sent as `options.num_ctx` when set to a positive integer; otherwise omitted and the model/server default applies |
| `RECON_MODEL_TIMEOUT` | `5m` | Maximum duration of one model request, using Go duration syntax such as `90s` or `10m`; local timeouts are not retried |
| `SYSTEM_PROMPT` | unset | Optional path to the initial base prompt |
| `RECON_API_ADDRESS` | unset | Run the HTTP API at this address instead of the terminal |
| `RECON_API_RUN_RETENTION_MINUTES` | `30` | Minutes after a run reaches a terminal status (`completed`/`failed`/`cancelled`/`limit_reached`) before it is evicted from the in-memory API run registry; `0` disables eviction. The on-disk snapshot remains the audit record either way |

All Ollama requests go through a shared HTTP client (`ollamaHTTPClient` in [runtime.go](runtime.go)). If `OLLAMA_URL` uses plain HTTP to a non-loopback host, a startup warning is printed since prompts and responses would cross the network unencrypted and readable/alterable by a network attacker; use `https://` (optionally with `OLLAMA_CA_CERT`) or an encrypted tunnel instead.


The application sends non-streaming chat requests. Agent calls set `format: "json"`; thinking is controlled with `/think-on` and `/think-off`. A model response that is cut off mid-JSON (e.g. a long code answer hitting the model's own output limit) is detected and treated as a revision request asking for a shorter, complete answer, rather than spending one of the run's 3 consecutive-failure strikes.

## Diagnostics

`-check-ollama` queries the configured `OLLAMA_URL` host's `/api/tags` endpoint, prints reachability and model count, and exits without starting the terminal or API. The terminal `/ollama` command runs the same check interactively during a session.

`-check-ollama-ps` queries the host's `/api/ps` endpoint, prints currently loaded models with size and expiry, and exits the same way. The terminal `/ollama-ps` command runs the same check interactively.

`-check-ollama-models` queries the host's `/api/tags` endpoint, prints available models with size and modified time, and exits the same way. The terminal `/ollama-ls` command runs the same check interactively.

## Terminal Line Editing

On macOS and Linux, interactive stdin prompts (`[goal]:`, clarification, revision feedback, structured `/agent` fields) support arrow-key cursor movement and up/down history recall, implemented with direct termios ioctl calls (`termios_darwin.go`, `termios_linux.go`) and no external dependency. History is persisted to `~/.recon/history`. Non-interactive or piped stdin automatically falls back to plain line reading, preserving existing scripted and test behavior.

## Terminal Lifecycle

Plain terminal input starts a run using that text as the objective. `/agent [GOAL]` collects the full `AgentGoalSpec`: objective, expected output, constraints, completion criteria, and optional attachment path.

The selected system prompt is captured as the immutable base prompt for each run and combined with the embedded agent protocol. An attachment remains at its existing local path. Supported image attachments are base64-encoded into Ollama messages for worker and critic vision analysis, capped at 20 MiB. Other attachments have their text extracted via `fileextract.go` (PDF, DOCX, PPTX, DOC, PCAP/PCAPNG/CAP, and plain text, capped at 20 MiB) and share a 64 KiB inline-text budget per run. Files attached through a directory initially include at most a 1 KiB excerpt each; metadata and paths remain in the prompt so the worker can inspect selected files with allowed tools. If extraction fails or the file is too large, the note names the path for tool-based inspection. A plain objective can infer an existing absolute image path, while `/agent` and the API accept explicit attachment paths. `/attach` persists a file or directory's regular files across subsequent terminal runs. `/deattach`/`/detach` clears all persistent attachments, or a single one when given a path; `/attached` lists the current set. At most 30 files are attached per run. Attached file metadata is carried on the run as `AttachmentNote` and included in the objective seen by the worker and critic; images are additionally sent as native Ollama message content.

The worker runs synchronously and model responses are non-streaming. The terminal immediately displays a working status with the configured model-request timeout, then emits plans, commands, observations, errors, and verification feedback as they occur. Ctrl+C cancels the active context and run-owned jobs. The worker pauses in:

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

The in-memory API registry is process-local. Persisted snapshots remain audit records and are not loaded after restart. The API has no authentication and must remain loopback-bound or be protected by a trusted authenticated proxy. Runs that reach a terminal status (`completed`/`failed`/`cancelled`/`limit_reached`) are evicted from the registry after `RECON_API_RUN_RETENTION_MINUTES` (default 30, `0` disables eviction); active and waiting runs are never evicted. Eviction is swept opportunistically on `POST /api/agent/runs` and `GET|POST /api/agent/runs/{id}...`, not via a background timer.

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
- `list_directory`
- `search_files`
- `update_findings`
- `request_clarification`
- `answer`

The decoder requires exactly one JSON object and rejects unknown fields or trailing content. Tool failures become observations so the agent can recover.

`list_directory` and `search_files` are native Go implementations, not shell commands; they bypass the command allowlist entirely and cannot fail on shell syntax. `list_directory` walks a directory up to `maxDepth` (default and max 6), capped at 2000 entries. `search_files` runs a Go regular expression across a file or a directory's text files (skipping binaries and files over 2 MiB), capped at 200 matches.

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

Do not claim the allowlist is a security boundary. Use recon only in a trusted environment and only for authorized targets. Instances/runs share no coordination — do not point multiple instances at the same target or attachment path without external coordination.

## Product Invariants

- The application is agent-only, with terminal and opt-in headless API interfaces.
- Preserve the worker-owned loop, planning, critic, retries, context compaction, evidence, jobs, attachments, clarification, revision, approval, cancellation, and limits.
- Keep agent actions strict and machine-readable.
- Keep bundled prompts embedded so the binary remains standalone.
- Keep run snapshots compatible when practical.
- Do not add a Web UI without an explicit product change.

</application_context>
