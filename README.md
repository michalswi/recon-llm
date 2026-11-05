# recon

`recon` is an autonomous agent for an Ollama-compatible chat endpoint, with a terminal interface and an opt-in HTTP API but no browser UI. Each objective runs through a strict JSON action loop with planning, allowlisted command execution, background jobs, evidence tracking, context compaction, and independent answer verification.

## Requirements

- Go 1.25.5 or a prebuilt `recon` binary
- A running [Ollama](https://ollama.com/) server
- A local model; larger models generally follow the agent protocol more reliably

```sh
ollama serve
ollama pull qwen3.5:0.8b
make build
./recon
```

All bundled prompts are embedded in the binary, so the executable can run from any directory. `SYSTEM_PROMPT` may still point to an external base-prompt file.

Check that Ollama is reachable at any time with:

```sh
./recon -check-ollama
```

This exits `0` and prints the reachable URL and model count, or exits `1` with the connection error.

List models currently loaded in Ollama with:

```sh
./recon -check-ollama-ps
```

List models available in Ollama with:

```sh
./recon -check-ollama-models
```

## Terminal Line Editing

When stdin is an interactive terminal (macOS or Linux), the `[goal]:` prompt and other free-text prompts support left/right arrow cursor movement, backspace, and up/down arrow history navigation across the session. History is persisted to `~/.recon/history` (capped at 500 entries) and shared across runs. Piped or non-interactive input falls back to plain line reading automatically.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `OLLAMA_URL` | `http://localhost:11434/api/chat` | Ollama-compatible chat endpoint |
| `OLLAMA_MODEL` | `qwen3.5:0.8b` | Model used by the worker, critic, and compactor |
| `OLLAMA_CA_CERT` | unset | Path to a PEM CA certificate to trust, for a self-signed cert on a TLS-terminating Ollama reverse proxy; only takes effect when `OLLAMA_URL` uses `https://` |
| `OLLAMA_INSECURE_SKIP_VERIFY` | `false` | Disable TLS certificate validation for Ollama connections; ignores `OLLAMA_CA_CERT`. Testing only — a network attacker can impersonate the endpoint undetected |
| `OLLAMA_NUM_PREDICT` | `-1` (no limit) | Sent as `options.num_predict` on every request, so a model's own default output-length limit can't silently truncate long answers |
| `OLLAMA_NUM_CTX` | unset | Sent as `options.num_ctx` when set to a positive integer; otherwise the model/server default is used |
| `RECON_MODEL_TIMEOUT` | `5m` | Maximum duration of one model request, using Go duration syntax such as `90s` or `10m`; local timeouts are not retried |
| `RECON_MAX_ITERATIONS` | `30` | Iterations per execution segment; must be a positive integer, otherwise defaults to 30 |
| `SYSTEM_PROMPT` | built-in prompt | Optional path to the agent base prompt |
| `RECON_API_ADDRESS` | unset | Run the HTTP API at this address instead of the terminal |
| `RECON_API_RUN_RETENTION_MINUTES` | `30` | Minutes after a run reaches a terminal status before it's evicted from the in-memory API run registry; `0` disables eviction |

If `OLLAMA_URL` uses plain HTTP to a non-loopback host, recon prints a startup warning: prompts and responses would cross the network unencrypted. Use `https://` (with `OLLAMA_CA_CERT` for a private CA) or an encrypted tunnel instead.


## Terminal Workflow

Enter an objective directly to start a run. An existing absolute `.jpg`, `.jpeg`, `.png`, or `.webp` path in the objective is attached directly to Ollama for visual analysis. Image analysis requires a vision-capable model.

Use `/attach PATH` to attach a file or the regular files in a directory to every subsequent run in the current terminal process. Up to 30 files can be active. Image attachments are capped at 20 MiB. Non-image attachments are capped at 20 MiB for extraction and share a 64 KiB inline-text budget per run; directory members initially include at most a 1 KiB excerpt each, with their paths preserved for targeted inspection by the agent. Run `/attached` to list them, `/deattach PATH` (or `/detach PATH`) to remove one, or `/deattach`/`/detach` with no argument to clear all. Use `/agent [GOAL]` for the structured form, which also accepts expected output, constraints, completion criteria, a one-run attachment path, and an optional writable root for requested file creation.

The agent immediately prints a working status before its first non-streaming model request, including the configured request timeout. It then prints plans, commands, observations, and critic feedback as they occur. It pauses when it needs clarification or when a verified candidate answer is ready. A candidate can be accepted, revised with feedback, or stopped. Press Ctrl+C to cancel an active run and its owned background jobs.

Exhausting the iteration budget pauses the run with a continue [c] or stop [s] prompt. Continuing, clarifying, or revising grants a fresh segment budget and a new 30-minute deadline while retaining the lifetime iteration count and run history. Deadline expiry remains terminal. Background `check_job`/`get_job` actions wait for completion without further model calls or iterations; cancellation and the segment deadline still apply. Jobs remain active during user-decision pauses, subject to their separate 1-hour timeout, and are cleaned up when the run ends.

Run `/h` for all terminal controls, including `/ollama` to re-check server reachability, `/ollama-ps` to list currently loaded models, and `/ollama-ls` to list available models, without restarting. Prompt selection and thinking mode apply to subsequent runs.

## Agent API

Run the headless API explicitly with a loopback address:

```sh
./recon -api-address 127.0.0.1:8080
```

`RECON_API_ADDRESS=127.0.0.1:8080 ./recon` is equivalent. Create a run and inspect its status with:

```sh
curl -sS -X POST http://127.0.0.1:8080/api/agent/runs \
  -H 'Content-Type: application/json' \
  -d '{"goal":"create main.go in /tmp/recon-test","writableRoots":["/tmp/recon-test"]}'

curl -sS http://127.0.0.1:8080/api/agent/runs/<run-id>
```

The API follows the same worker and critic lifecycle as the terminal:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/health` | Check server availability |
| `POST` | `/api/agent/runs` | Create and start a run |
| `GET` | `/api/agent/runs/{id}` | Read run status, events, and candidate output |
| `POST` | `/api/agent/runs/{id}/feedback` | Submit `{"feedback":"..."}` for clarification or revision |
| `POST` | `/api/agent/runs/{id}/approve` | Accept a verified candidate |
| `POST` | `/api/agent/runs/{id}/continue` | Explicitly resume `waiting_continue` with fresh iteration/time budgets; no request body |
| `POST` | `/api/agent/runs/{id}/cancel` | Cancel a live or waiting run |

Each response includes `sessionId`, which names the persisted snapshot at `~/.recon/agent_runs/<sessionId>.json`.

`writableRoots` is optional and opt-in. Each entry must be an existing directory. When present, the native `write_file` action may create complete multiline files beneath those roots with a 256 KiB per-file limit and aggregate limits of 50 files and 1 MiB per run. It rejects symlink escapes and existing destinations unless overwrite is explicit. File creation alone never authorizes building, testing, or running generated code.

Responses expose `iteration` (lifetime total), `segmentIteration` (current segment), and `maxIterations` (per-segment limit). Iteration exhaustion produces `waiting_continue`; only `/continue` or `/cancel` applies to that pause. `/feedback` remains for clarification and revision, and terminal `limit_reached` runs cannot be continued.

Runs that reach a terminal status (`completed`/`failed`/`cancelled`/`limit_reached`) are evicted from the in-memory registry after `RECON_API_RUN_RETENTION_MINUTES` (default 30); active and waiting runs are never evicted, and the on-disk snapshot is unaffected either way.

The API has no authentication and can initiate commands through the agent. It is disabled by default; keep it bound to loopback unless it is protected by a trusted authenticated proxy.

## Agent Guarantees

- Strict single-object JSON actions with unknown fields rejected
- Default 30 iterations per segment, configurable with `RECON_MAX_ITERATIONS`; exhaustion pauses for explicit continuation
- A 30-minute active-segment deadline; expiry is terminal
- Configurable model-request timeout (default 5 minutes) and a two-minute foreground-command deadline
- A 4096-token recent-event budget with semantic compaction
- Command observations truncated to 32 KiB before model reuse
- Three consecutive model or protocol failures before failure
- Independent critic verification before user approval
- Evidence requirements for completed plan steps and explicit command goals; a pending plan step is advisory bookkeeping and does not by itself override an evidence-backed critic approval
- Revision and clarification feedback retained as first-class context
- Background jobs scoped to their owning run and cancelled during cleanup
- Deterministic, native `list_directory` and `search_files` actions for exploring directories with many files, without shell syntax
- Native, writable-root-confined `write_file` for requested files, with per-file atomic writes and metadata-only evidence

Run snapshots are written to `~/.recon/agent_runs/<run-id>.json`. Attachments are referenced by their existing local path and are never copied or deleted by recon.

## Docker

Docker support is temporary and not currently developed or maintained. The [Dockerfile](Dockerfile) and Docker build target in the [Makefile](Makefile) are commented out; build and run the Go application directly for now. Any previously published image may not reflect the current application.

## Security

Command execution is not a sandbox. The agent runs commands with the filesystem and network permissions of the recon process. Use it only in a trusted environment and only against systems you are authorized to access. Instances/runs share no coordination — do not point multiple instances at the same target or attachment path without external coordination.

Licensed under the [Apache License 2.0](LICENSE).
