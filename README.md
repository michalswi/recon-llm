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

Keep `prompts/` beside the binary because `prompts/agent.txt` is a runtime dependency.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `OLLAMA_URL` | `http://localhost:11434/api/chat` | Ollama-compatible chat endpoint |
| `OLLAMA_MODEL` | `qwen3.5:0.8b` | Model used by the worker, critic, and compactor |
| `SYSTEM_PROMPT` | built-in prompt | Optional path to the agent base prompt |

## Terminal Workflow

Enter an objective directly to start a run. Use `/agent [GOAL]` for the structured form, which also accepts expected output, constraints, completion criteria, and an optional local attachment path.

The agent prints plans, commands, observations, and critic feedback as they occur. It pauses when it needs clarification or when a verified candidate answer is ready. A candidate can be accepted, revised with feedback, or stopped. Press Ctrl+C to cancel an active run and its owned background jobs.

Run `/h` for all terminal controls. Prompt selection and thinking mode apply to subsequent runs.

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
