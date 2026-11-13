<div align="center">

# recon

[![stars](https://img.shields.io/github/stars/michalswi/recon-llm?style=for-the-badge&color=353535)](https://github.com/michalswi/recon-llm)
[![forks](https://img.shields.io/github/forks/michalswi/recon-llm?style=for-the-badge&color=353535)](https://github.com/michalswi/recon-llm/fork)
[![releases](https://img.shields.io/github/v/release/michalswi/recon-llm?style=for-the-badge&color=353535)](https://github.com/michalswi/recon-llm/releases)

`recon` is an autonomous terminal agent with an optional HTTP API, powered by an Ollama-compatible chat endpoint. It uses strict JSON actions to plan tasks, run allowlisted commands, manage background jobs, track evidence, compact context, and independently verify answers.
</div>

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

Bundled prompts are embedded, so the binary runs from any directory. Use `SYSTEM_PROMPT` for an external base prompt.

Check Ollama without starting a session:

```sh
./recon -check-ollama         # Reachability and model count
./recon -check-ollama-ps      # Currently loaded models
./recon -check-ollama-models  # Available models
```

`-check-ollama` prints the reachable URL and model count and exits `0`, or prints the connection error and exits `1`.

## Terminal Line Editing

On macOS and Linux, interactive prompts support left/right arrows, backspace, and up/down history navigation. History is saved to `~/.recon/history` (up to 500 entries) and shared across runs. Piped or non-interactive input uses plain line reading.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `OLLAMA_URL` | `http://localhost:11434/api/chat` | Ollama-compatible chat endpoint |
| `OLLAMA_MODEL` | `qwen3.5:0.8b` | Model used by the worker, critic, and compactor |
| `OLLAMA_CA_CERT` | unset | PEM CA certificate to trust, e.g. for a self-signed Ollama reverse proxy; HTTPS only |
| `OLLAMA_INSECURE_SKIP_VERIFY` | `false` | Disable TLS certificate validation and ignore `OLLAMA_CA_CERT`. Testing only: attackers can impersonate the endpoint |
| `OLLAMA_NUM_PREDICT` | `-1` (no limit) | Set `options.num_predict` on every request, overriding the model's default output limit |
| `OLLAMA_NUM_CTX` | unset | Set `options.num_ctx` to a positive integer; otherwise use the model/server default |
| `RECON_MODEL_TIMEOUT` | `5m` | Per-request timeout, e.g. `90s` or `10m`; local timeouts are not retried |
| `RECON_MAX_ITERATIONS` | `30` | Iterations per execution segment; invalid or nonpositive values use 30 |
| `SYSTEM_PROMPT` | built-in prompt | Optional path to the agent base prompt |
| `RECON_API_ADDRESS` | unset | Run the HTTP API at this address instead of the terminal |
| `RECON_API_RUN_RETENTION_MINUTES` | `30` | Minutes to retain finished API runs in memory; `0` disables eviction |

Plain HTTP to a non-loopback host triggers a startup warning: prompts and responses travel unencrypted. Use HTTPS (with `OLLAMA_CA_CERT` for a private CA) or an encrypted tunnel.

## Terminal Workflow

Enter an objective to start a run. An existing absolute `.jpg`, `.jpeg`, `.png`, or `.webp` path in the objective is automatically attached for analysis by a vision-capable model.

- `/agent [GOAL]` opens a guided task form (examples below).
- `/attach PATH` attaches a file or a directory's regular files to subsequent runs in the current terminal process.
- `/attached` lists attachments.
- `/detach PATH` removes one attachment; `/detach` clears all. `/deattach` is an alias.

Type `Explain DNS` to start immediately, or `/agent Explain DNS` to add details first. `/agent` on its own also asks for the goal. `[GOAL]` means optional text; do not type the brackets.

The form asks for expected output, constraints, completion criteria, an attachment, a folder where recon may write files, and comma-separated skill names. Press Enter to skip any optional field. Both ways start the same agent.

Up to 30 files can be attached. Images and document extraction are capped at 20 MiB per file. Extracted text shares a 64 KiB budget per run; directory files initially include at most 1 KiB each, with paths retained for further inspection.

The agent shows a working status and request timeout before its first non-streaming model call, then displays plans, commands, observations, and critic feedback. It pauses for clarification or a verified answer, which you can accept, revise, or reject by stopping. Ctrl+C cancels the active run and its background jobs.

At the iteration limit, choose continue [c] or stop [s]. Continuing, clarifying, or revising renews the iteration budget and 30-minute deadline, preserving history and the lifetime iteration count. Deadline expiry ends the run.

Background `check_job`/`get_job` actions wait without extra model calls or iterations; cancellation and the segment deadline still apply. Jobs continue during user-decision pauses, have a separate 1-hour timeout, and are cleaned up when the run ends.

Use `/h` for all controls. During a session, `/ollama` checks reachability, `/ollama-ps` lists loaded models, and `/ollama-ls` lists available models. Prompt and thinking-mode changes apply to subsequent runs.

## External Skills

Think of a skill as a **saved checklist for the agent**. Instead of typing the same instructions every time, save them in a file and reuse them.

For example, you often ask recon to explain application logs. A skill can tell it what to look for and how to organize the answer.

### Example: explain application errors

Create the folder `~/.recon/skills/log-review/`, then save this text as `SKILL.md` inside it:

```markdown
---
name: log-review
description: Use when the user asks to investigate application logs or explain errors.
---
# Review an application log

1. Read the log supplied by the user. If none was supplied, ask for it.
2. Find errors and repeated warnings. Include timestamps when available.
3. Explain the likely cause in plain language. Distinguish what the log
   proves from what is only a guess.
4. Suggest the next checks, but do not run them or change files unless asked.

Organize the answer into:
- Summary: what appears to be going wrong.
- Evidence: short relevant log excerpts, with secrets removed.
- Next steps: checks the user can take to confirm or fix the problem.
```

The header between `---` lines tells recon the skill's **name** and **when to use it**. Everything below it tells the agent **what to do**. The name must match the folder name.

### Use it in recon

1. Type `/skills` to check that `log-review` is listed.
2. Type `/agent Explain the errors in my application log`.
3. At **Attachment path**, enter the path to your actual log file.
4. At **Skills**, enter `log-review`. Press Enter to skip the other optional fields.

Recon will read your log and use the checklist to produce a summary, evidence, and next steps. The skill is not a script: the agent follows its instructions using recon's existing tools.

You can also leave the Skills field empty and let the agent choose based on skill descriptions. API users can select it with `"skills":["log-review"]`.

No rebuild is needed. Edit the file to change the checklist; changes apply to new runs, not a run already in progress.

### Prompts vs. Skills

| Prompts | Skills |
|---|---|
| Set a role: "You are a cloud engineer" | Give a checklist: "Review logs using these steps" |
| Built-in defaults, with external/custom overrides | External files; no rebuild needed |
| One base prompt per run | Multiple skills per run |

**File rules:** the header accepts only `name` and `description`. Names use lowercase letters, digits, and single separating hyphens (up to 64 characters). Descriptions can be up to 512 bytes; instructions must not be empty.

**Limits:** 64 installed skills, 16 KiB per file, and 8 active skills totaling at most 64 KiB of instructions. Invalid skill files prevent a run from starting.

**Trust:** use only reviewed skills, without secrets: active instructions go to Ollama and local run snapshots. Skills do not grant extra permissions or override the agent protocol. This version does not automatically run scripts, load supporting files, or download skills. See [APP.md](APP.md#external-skills) for lifecycle details.

## Agent API

Start the headless API on loopback:

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

Run responses include `sessionId`, identifying the snapshot at `~/.recon/agent_runs/<sessionId>.json`.

Optional `writableRoots` entries must be existing directories. They authorize native `write_file` actions beneath those roots, limited to 256 KiB per file, 50 files and 1 MiB per run. Files may contain multiline text; symlink escapes are rejected, and overwriting requires explicit permission. **File creation does not authorize building, testing, or running generated code.**

Responses expose `iteration` (lifetime total), `segmentIteration` (current segment), and `maxIterations` (segment limit). At `waiting_continue`, only `/continue` or `/cancel` applies; `/feedback` is for clarification and revision. Terminal `limit_reached` runs cannot resume.

Finished runs (`completed`/`failed`/`cancelled`/`limit_reached`) leave the in-memory registry after the configured retention period. Active and waiting runs, and on-disk snapshots, are retained.

The API is disabled by default, has no authentication, and can execute commands through the agent. Keep it on loopback or behind a trusted authenticated proxy.

## Agent Guarantees

- Strict single-object JSON actions with unknown fields rejected
- Default 30 iterations per segment, configurable with `RECON_MAX_ITERATIONS`; exhaustion pauses for explicit continuation
- A 30-minute active-segment deadline; expiry is terminal
- Configurable model-request timeout (default 5 minutes) and a two-minute foreground-command deadline
- A 4096-token recent-event budget with semantic compaction
- Command observations truncated to 32 KiB before model reuse
- Run ends after three consecutive model or protocol failures
- Independent critic verification before user approval
- Evidence required for completed plan steps and explicit command goals; pending steps alone do not block critic approval
- Revision and clarification feedback retained in context
- Background jobs scoped to their owning run and cancelled during cleanup
- Native `list_directory` and `search_files` actions, without shell syntax
- Native `write_file`, restricted to authorized roots, with atomic per-file writes and metadata-only evidence

Run snapshots are saved under `~/.recon/agent_runs/`. Attachments stay at their original paths; recon never copies or deletes them.

## Docker [feature is temporary disabled]

The image runs as an unprivileged user. Use an interactive terminal and an Ollama URL reachable from the container.

```sh
docker run --rm -it \
  -e OLLAMA_URL="http://<ollama-host>:11434/api/chat" \
  michalsw/recon:latest
```

Not every command listed in [comm.go](comm.go) is installed in the image.

## Security

**Command execution is not sandboxed.** Commands inherit recon's filesystem and network permissions. Use only in trusted environments against authorized systems. Runs and instances do not coordinate; externally coordinate access to shared targets or attachment paths.

Licensed under the [Apache License 2.0](LICENSE).
