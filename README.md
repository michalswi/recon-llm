# recon

`recon` is a terminal-only autonomous agent for an Ollama-compatible chat endpoint. Each objective runs through a strict JSON action loop with planning, allowlisted command execution, background jobs, evidence tracking, context compaction, and independent answer verification.

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
| `SYSTEM_PROMPT` | built-in prompt | Optional path to the agent base prompt |
| `RECON_API_ADDRESS` | unset | Run the HTTP API at this address instead of the terminal |

If `OLLAMA_URL` uses plain HTTP to a non-loopback host, recon prints a startup warning: prompts and responses would cross the network unencrypted. Use `https://` (with `OLLAMA_CA_CERT` for a private CA) or an encrypted tunnel instead.


## Terminal Workflow

Enter an objective directly to start a run. An existing absolute `.jpg`, `.jpeg`, `.png`, or `.webp` path in the objective is attached directly to Ollama for visual analysis. Image analysis requires a vision-capable model.

Use `/attach PATH` to attach a file or the regular files in a directory to every subsequent run in the current terminal process. Up to 16 files can be active. Run `/attached` to list them, `/deattach PATH` (or `/detach PATH`) to remove one, or `/deattach`/`/detach` with no argument to clear all. Use `/agent [GOAL]` for the structured form, which also accepts expected output, constraints, completion criteria, and a one-run attachment path.

The agent prints plans, commands, observations, and critic feedback as they occur. It pauses when it needs clarification or when a verified candidate answer is ready. A candidate can be accepted, revised with feedback, or stopped. Press Ctrl+C to cancel an active run and its owned background jobs.

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
  -d '{"goal":"inspect the current directory","constraints":"read only"}'

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
| `POST` | `/api/agent/runs/{id}/cancel` | Cancel a live or waiting run |

Each response includes `sessionId`, which names the persisted snapshot at `~/.recon/agent_runs/<sessionId>.json`.

The API has no authentication and can initiate commands through the agent. It is disabled by default; keep it bound to loopback unless it is protected by a trusted authenticated proxy.

## Agent Guarantees

- Strict single-object JSON actions with unknown fields rejected
- Maximum 30 iterations and 30 minutes per worker segment
- Two-minute model-call and foreground-command deadlines
- A 4096-token recent-event budget with semantic compaction
- Command observations truncated to 32 KiB before model reuse
- Three consecutive model or protocol failures before failure
- Independent critic verification before user approval
- Evidence requirements for completed plan steps and explicit command goals
- Revision and clarification feedback retained as first-class context
- Background jobs scoped to their owning run and cancelled during cleanup
- Deterministic, native `list_directory` and `search_files` actions for exploring directories with many files, without shell syntax

Run snapshots are written to `~/.recon/agent_runs/<run-id>.json`. Attachments are referenced by their existing local path and are never copied or deleted by recon.

## Docker

The image runs the terminal application as an unprivileged user. Use an interactive terminal and configure an Ollama URL reachable from the container.

```sh
docker run --rm -it \
  -e OLLAMA_URL="http://<ollama-host>:11434/api/chat" \
  michalsw/recon:latest
```

Not every command listed in [comm.go](comm.go) is installed in the image.

## Security

Command execution is not a sandbox. The agent runs commands with the filesystem and network permissions of the recon process. Use it only in a trusted environment and only against systems you are authorized to access.

Licensed under the [Apache License 2.0](LICENSE).
