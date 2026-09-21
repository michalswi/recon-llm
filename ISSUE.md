# Issue: agent burns its retry budget on JSON-escaped shell metacharacters

## Observed

Real session log (`gemma4:12b`, agent mode, `pentester.txt`):

```
Agent error: {"action":"run_command","command":"ffuf -u http://localhost:5050/FUZZ -w <(echo -e \"flag
admin
config
hidden
secret\")"}
Agent error: command must be a single physical line; heredocs and multi-line scripts are not supported. To deliver file or source content, use the answer action with the complete content in a fenced code block
```

This repeated 3 times with cosmetic variations (`echo -e`, `echo`, `tr -d`), all still producing the same
rejection, until the run exhausted its attempt budget and failed entirely:

```
Agent run failed: agent failed: command must be a single physical line; heredocs and multi-line scripts are not supported...
```

## Root cause

`runAgentCommand` executes every command via `bash -c command` (agent_worker.go:1299), so a technique like
process substitution — `ffuf -w <(echo -e "flag\nadmin\n...")` — is not conceptually forbidden and would
work fine as a real bash command *if written on one physical line*.

The model's actual mistake is a **JSON-escaping error**, not a conceptual one:

- To get a literal two-character sequence `\n` into the resulting bash command (so `echo -e` turns it into
  a real newline *at shell runtime*), the JSON string must contain an escaped backslash: `\\n`.
- The model instead emitted a single JSON escape `\n`. JSON decoding turns that into an actual `0x0A`
  newline character embedded in the Go `command` string.
- `validateAgentCommand` correctly rejects that: `strings.ContainsAny(command, "\n\r")` treats any embedded
  raw newline as a would-be multi-line/heredoc command (agent_worker.go:1017-1020).
- The model never diagnosed *why* it was rejected — it retried the same flawed escaping pattern three times,
  burned its consecutive-error budget, and the whole run failed instead of falling back to a working approach.

`prompts/agent.txt` (shared by every persona) states the single-physical-line/no-heredoc/no-embedded-newline
rule, but never explains:
1. The JSON double-escaping mechanics needed when the target shell syntax itself requires a backslash escape
   (e.g. `\n` for `echo -e`, `\t`, or a literal `\"`).
2. A safe fallback pattern for building multi-line input (wordlists, file contents) without needing an
   embedded newline in a single command at all.

This is a known weakness of small/local models (like `gemma4:12b`) generating JSON with nested
shell-escape sequences — they are especially prone to under-escaping backslashes.

## Solution

Add a short clarifying rule to `prompts/agent.txt` (cross-persona, so every prompt benefits):

- Explain the distinction explicitly: a raw newline character in the JSON `command` string is always
  rejected; a literal backslash-n meant for the shell (e.g. `echo -e`) must be double-escaped as `\\n` in
  JSON so it survives JSON decoding as the two characters `\` and `n`.
- Recommend a safer fallback for multi-line input construction that avoids the pitfall entirely: build the
  content across several single-line `echo "word" >> file` (or `printf`) commands, one per line, then
  reference the resulting file path in a later single-line command — instead of relying on process
  substitution or inline multi-value strings that require correct nested escaping on the first try.

This does not change `validateAgentCommand`'s behavior (the single-physical-line rule is intentional and
correct) — it only gives the model enough information to avoid repeating the same failing escape pattern
and to recover with a working alternative before exhausting its retry budget.

## Risk check: does the fallback conflict with existing rules?

`agent.txt` already contains a separate, pre-existing rule: *"Deliver file or source content only through
the answer action's fenced code block"* (line 34), reinforced by *"For code-generation goals, include the
complete source in a non-empty fenced code block in every candidate answer"* (line 38). This is what
`apps_developer.txt`-style code-generation runs rely on.

A loosely worded fallback ("build multi-line input via several single-line `echo >> file` commands") risks
being read as license to write source files piece-by-piece via shell commands instead of using `answer` —
which would regress the code-generation workflow (fragile quoting, no single deliverable, inconsistent with
the existing convention).

**Fix:** scope the fallback narrowly to *ephemeral tool input* (e.g., a throwaway wordlist fed to `ffuf`),
and explicitly restate that it never applies to deliverable source/file content, which still goes through
`answer` only:

> If a command genuinely needs multi-line data as scratch input to another tool (e.g. a small wordlist),
> build it via several single-line `echo "word" >> /tmp/file` commands, then reference the path in a later
> command — never as a substitute for delivering source/file content, which always goes through the answer
> action's fenced code block.

With that scoping, the two rules don't collide: the JSON-escaping clarification is purely additive/explanatory
(no risk to any persona), and the fallback is explicitly carved out from the existing file/source-delivery
rule rather than contradicting it.

## Review comments and concerns

The diagnosis is correct. JSON decoding turns `\n` in the model's JSON source into a physical newline, while
`\\n` survives decoding as the two characters needed by the shell. The validator should continue rejecting
physical newlines, and process substitution itself is not the problem because commands are executed by Bash.

The proposed prompt-only change is a reasonable first fix, with these adjustments:

- Describe the fallback behavior without prescribing `echo` or `printf`: for ephemeral multi-line tool input,
  create the scratch file incrementally using separate single-line actions, one logical line per action, then
  reference that path in a later action. This follows the prompt-design convention that the allowlist and
  execution environment determine the concrete command choice.
- Keep the explicit exclusion for deliverable source/file content. Scratch input for another tool is the only
  intended exception; deliverables still belong in the `answer` action's fenced code block.
- Avoid grouping a literal `\"` with the `\n` example without explaining its different encoding. If the shell
  must receive a backslash followed by a quote, the JSON source must contain `\\\"`. Omitting the quote example
  keeps the rule shorter and less error-prone for small models.
- Add validator tests that pass decoded Go strings directly: one containing the literal characters `\` and `n`
  must be accepted, while one containing an actual newline byte must be rejected. This locks down the intended
  boundary independently of prompt wording.

Production testing with `gemma4:12b` confirmed the residual concern: prompt guidance reduced ambiguity but did
not reliably prevent the model from emitting a decoded physical newline. A narrowly classified, one-time
revision response is therefore implemented for an embedded-newline command. It explains the escaping mistake
without spending a strike. Repeating the same error after that correction returns to normal strike handling,
so the model receives one extra recovery opportunity but cannot loop indefinitely.

