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
| `RECON_API_TOKEN` | unset | API: require `Authorization: Bearer <token>` on every request. Master mode (`-master`): token sent to every node |

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

For example, your goal is: "analyze output from pinging google.com five times and make report". A skill can tell recon which measurements to include and how to explain them.

### Example: ping a host and report the results

Create the folder `~/.recon/skills/ping-report/`, then save this text as `SKILL.md` inside it:

```markdown
---
name: ping-report
description: Use when the user asks to run ping and report on connectivity.
---
# Prepare a ping report

1. Run ping with the host and packet count requested by the user.
   On macOS/Linux, five pings to google.com use: ping -c 5 google.com
2. Read the actual command output. Never invent results.
3. Report packets sent, packets received, and packet loss percentage.
4. Include minimum, average, and maximum round-trip time when available.
5. Explain what these results show in plain language. Five pings are
   only a brief sample, not proof of overall connection reliability.
6. If ping fails, report the error. Missing replies alone do not prove
   the host is offline; ICMP traffic may be blocked.
7. Do not run additional network checks unless the user asks.
```

The header between `---` lines tells recon the skill's **name** and **when to use it**. Everything below it tells the agent **what to do**. The name must match the folder name.

### Use it in recon

**Skills are off by default for ordinary typed goals.** You do not need `/agent` to enable them. Type `/skills` to check that `ping-report` is installed, then enable automatic selection:

```text
/skill auto
analyze output from pinging google.com five times and make report
```

In auto mode, the agent can choose and load `ping-report` based on its description. Automatic selection is the agent's decision; it is not guaranteed for every goal.

**To choose the skill yourself without a form:**

```text
/skill attach ping-report
analyze output from pinging google.com five times and make report
```

- `/skill attach NAME` adds a skill for subsequent typed goals. While any skills are attached, only those skills are used and automatic selection is disabled.
- `/skill detach NAME` removes one; `/skill detach` removes all. Detaching the last skill turns skills off.
- `/skill off` disables skills and clears attached names.
- `/skill auto` enables automatic selection and clears attached names.
- `/skill list` (or `/skills`) shows available skills, attached names, and selection status.

Modes and attachments last for the current terminal session and affect new runs only. A new terminal session starts with skills off. These settings do not change `/agent` or API requests.

**Alternatively**, select a skill for one run using the optional `/agent` form:

1. Type `/agent analyze output from pinging google.com five times and make report`.
2. Press Enter to skip the optional fields until **Skills**. No attachment or writable folder is needed.
3. At **Skills**, enter `ping-report`.

Recon will use the checklist to run five pings and prepare a report from the actual output. The skill is not a script: the agent follows its instructions using recon's existing tools. The report appears in the terminal for you to accept or revise.

Leaving the form's Skills field empty also allows automatic selection. API users can explicitly select it with `"skills":["ping-report"]`.

No rebuild is needed. Edit the file to change the checklist; changes apply to new runs, not a run already in progress.

### Prompts vs. Skills

| Prompts | Skills |
|---|---|
| Set a role: "You are a network engineer" | Give a checklist: "Run ping and report these measurements" |
| Built-in defaults, with external/custom overrides | External files; no rebuild needed |
| One base prompt per run | Multiple skills per run |

**File rules:** the header accepts only `name` and `description`. Names use lowercase letters, digits, and single separating hyphens (up to 64 characters). Descriptions can be up to 512 bytes; instructions must not be empty.

**Limits:** 64 installed skills, 16 KiB per file, and 8 active skills totaling at most 64 KiB of instructions. Invalid skill files are skipped with a warning; explicitly selecting one fails with the reason. If `OLLAMA_NUM_CTX` is set, recon warns when estimated active-skill tokens reach 25% of it (advisory; the rest of the prompt is not counted).

**Trust:** use only reviewed skills, without secrets: active instructions go to Ollama and local run snapshots. Skills do not grant extra permissions or override the agent protocol. This version does not automatically run scripts, load supporting files, or download skills. See [APP.md](APP.md#external-skills) for lifecycle details.

## Agent API

Start the headless API on loopback:

```sh
./recon -api-address 127.0.0.1:8080
```

`RECON_API_ADDRESS=127.0.0.1:8080 ./recon` is equivalent. Create a run and inspect its status with:

```sh
curl http://127.0.0.1:8080/api/health
# {"status":"ok"}

curl -sS -X POST http://127.0.0.1:8080/api/agent/runs \
-H 'Content-Type: application/json' \
-d '{"goal":"Explain DNS"}'

curl -sS http://127.0.0.1:8080/api/agent/runs/<run-id>

curl -sS http://127.0.0.1:8080/api/agent/runs/<run-id> | jq '.finalAnswer'
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

The API is disabled by default and can execute commands through the agent. Without `RECON_API_TOKEN` it has no authentication: keep it on loopback or behind a trusted authenticated proxy (binding beyond loopback without a token prints a warning). With `RECON_API_TOKEN` set, every request, including `/api/health`, must send `Authorization: Bearer <token>`; use HTTPS or an encrypted tunnel so the token and results are not exposed on the network.

### Example: use a skill over the API

This reuses the `ping-report` skill from [External Skills](#external-skills). Unlike terminal typed goals, API runs have no `/skill` mode: list the skill names in `skills` to activate them for that run. Omit `skills` and the agent may still pick one itself from the catalog, but that is the agent's decision and less reliable with small models.

```sh
# 1. Create a run with the skill already active
curl -sS -X POST http://127.0.0.1:8080/api/agent/runs \
  -H 'Content-Type: application/json' \
  -d '{
    "goal": "Ping google.com five times and write a connectivity report from the actual output.",
    "skills": ["ping-report"],
    "expectedOutput": "Packets sent/received, loss %, min/avg/max RTT, plain-language interpretation.",
    "constraints": "Run only the ping needed. No additional network checks."
  }'

# 2. Poll until status is waiting_approval (the verified report is in the response)
curl -sS http://127.0.0.1:8080/api/agent/runs/<run-id>

# 3. Accept the report
curl -sS -X POST http://127.0.0.1:8080/api/agent/runs/<run-id>/approve
```

The creation response lists `activeSkills` (explicitly selected) and `availableSkills` (the loadable catalog), as names and descriptions only. The run's events show the agent running `ping -c 5 google.com`, followed by a candidate report based on that real output.

An unknown or invalid skill name returns `400` with the reason, and runs are created even if other installed skills are invalid; skipped skills appear as `warning` events. If `OLLAMA_NUM_CTX` is set, a `warning` event also appears when active skills use at least 25% of it.

## Master Mode

By default recon is a standalone agent. Start it with `-master` and a comma-separated list of recon API URLs to make it a **master** that drives many instances at once:

```sh
# on each worker host (they run the agent and talk to their own Ollama)
RECON_API_TOKEN=<token> ./recon -api-address 0.0.0.0:8080

# on the master (OLLAMA_* configures the orchestrator's own model, not the nodes')
OLLAMA_URL=http://localhost:11434/api/chat OLLAMA_MODEL=<capable-model> \
RECON_API_TOKEN=<token> ./recon -master http://10.0.0.5:8080,https://recon.example.com/node2
```

Without a token (trusted network only, e.g. a lab, or loopback with SSH tunnels), omit `RECON_API_TOKEN` on both sides:

```sh
# on each worker host: bind to loopback and reach it through an SSH tunnel
./recon -api-address 127.0.0.1:8080

# on the master, with tunnels such as: ssh -N -L 8081:127.0.0.1:8080 user@host1
OLLAMA_MODEL=<capable-model> ./recon -master http://127.0.0.1:8081,http://127.0.0.1:8082
```

A node bound beyond loopback without a token (e.g. `0.0.0.0:8080`) prints a warning: anyone who can reach the port can make it run commands.

The master uses its own Ollama model (`OLLAMA_URL`, `OLLAMA_MODEL`, ... read from the master's environment, default `localhost:11434`) as the **orchestrator**; each node uses the Ollama configured in its own environment. A running Ollama is required on the master for objectives, but not for `/broadcast`; if it is unreachable, the master says so immediately instead of retrying. `-master` cannot be combined with `-api-address`. Nodes are numbered `n1`, `n2`, ... in the order given.

**Orchestrator (default).** A typed objective is handled by an LLM orchestrator that works through six strict JSON actions: `list_nodes`, `dispatch_run`, `check_run`, `respond_run`, `request_clarification` and `answer`.

- **Splitting:** it breaks the objective into self-contained subtasks and dispatches each to a chosen node as a normal API run (`POST /api/agent/runs`). Each node uses its own model, worker, critic, skills, allowlist and limits. A node sees only the goal it is sent, not the objective or other nodes' results.
- **Waiting:** `check_run` waits for a run to leave `running` without extra model calls and relays the node's commands, warnings and errors live.
- **Review:** it accepts a node's verified candidate, sends specific feedback (also used to answer a node's clarification), continues a paused run, or cancels it.
- **Merge:** it returns one answer. An independent master critic checks that every claim is supported by the node results shown and that failed or unreachable runs are reported; you then accept, revise or stop, as in the terminal.
- **Your approval:** every dispatch is shown and needs your `y`/`n`, or `a` to allow all dispatches for the current objective. Declined dispatches are never sent. The orchestrator may also ask you clarifying questions.
- **Cleanup:** runs that are not accepted are cancelled when the orchestration ends, on Ctrl+C or SIGTERM, and on errors.
- **Limits:** at most 16 runs per objective, `RECON_MAX_ITERATIONS` model steps per segment (then continue [c] or stop [s]), and three consecutive invalid model responses end the orchestration.

**Broadcast.** `/broadcast GOAL` skips the orchestrator and sends the same goal to every node. Candidate approval, clarification and continuation are asked per node, and a summary table plus every accepted answer is printed. It works even if the master's Ollama is unreachable.

Other commands: `/nodes` checks reachability and that the token is accepted, `/m` shows the orchestrator model, `/h` shows help and `/q` exits. Ctrl+C cancels the active runs on all nodes. An unreachable node or one that rejects a run never stops the others.

### Prompts and skills in master mode

The master does not push prompts or skills to nodes; each node uses what is configured on its own host.

| | Master | Nodes |
|---|---|---|
| **Base prompt** | `SYSTEM_PROMPT` has no effect on the orchestrator, which always uses the embedded `prompts/master.txt` protocol | Chosen when the node process starts (`SYSTEM_PROMPT`, otherwise the built-in default). The API has no per-run prompt field, so the master cannot change it |
| **Skills** | Not used by the master itself, and it cannot list or attach a node's skills | Read from the node's own `~/.recon/skills/`. Runs started by the master do not select skills explicitly, so each node's agent may pick one automatically |

Consequences:
- To give a node a role, start it with its prompt, e.g. `SYSTEM_PROMPT=/path/pentester.txt ./recon -api-address 127.0.0.1:8080`, and install any skills on that host.
- Automatic selection is the agent's decision and is unreliable with small models. A node may answer with a skill name instead of a valid action (e.g. `{"action":"ping-report"}`) and burn its iterations. Prefer explicit goals ("run `ping -c 5 google.com` and report ...") and a capable model on the nodes, or remove skills from nodes that should not use them.
- To run a skill explicitly, call that node's API directly with `"skills":["name"]` (see [Example: use a skill over the API](#example-use-a-skill-over-the-api)) instead of going through the master.

Explicit per-run skills and prompts from the master would need new request fields and are not implemented.

**Security:** the master can make every node execute commands, and the orchestrator's goals are written by a model. Always set `RECON_API_TOKEN` (the same value on all nodes; it is sent as a bearer token) and use `https://` through a trusted reverse proxy or an encrypted tunnel for non-loopback nodes; the master warns about plain HTTP. Redirects are never followed, node output is treated as untrusted data in the orchestrator prompt, and control characters in it are stripped before display. Dispatched runs have no writable roots, attachments or explicit skills; nodes may pick skills automatically. The master does not deduplicate work: do not point several nodes at the same target or attachment path without external coordination. The orchestration transcript is not persisted by the master; each node keeps its own run snapshots. Small models struggle with this protocol, so use a capable model for the orchestrator.

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
