# Explicit Validation and Execution of Generated Code

## Scope

This document covers building, testing, running, starting, and probing generated code after files have been written. File creation itself is specified separately in `GO.md`.

Writing requested files does not authorize any command that compiles or executes them. The default completion point for a code-generation goal is successful creation of the requested files.

## Required User Intent

The agent may validate or execute generated code only when the objective, constraints, completion criteria, or later user feedback explicitly requests the relevant operation.

Examples of explicit authorization:

- "Create the Go application and build it."
- "Write these files, then run the tests."
- "Start the server and verify `/health`."
- "Compile the project but do not run it."

Examples that authorize writing only:

- "Generate a Go web application in `/tmp/example`."
- "Create `main.go` and `go.mod`."
- "Write the requested implementation to disk."

The worker must not infer build, test, run, start, or probe authorization merely because source files were created or a toolchain is available.

## Operation Boundaries

Authorization should be operation-specific:

- **Build** permits compilation but not execution of the resulting program.
- **Test** permits the selected test command; tests may execute arbitrary project code and therefore require explicit authorization.
- **Run/start** permits executing generated code and must define cancellation and timeout behavior.
- **Probe** permits checking a started process through explicitly requested local or network interactions.

Authorization for one operation does not imply authorization for the others. For example, "build it" does not permit `go test`, `go run`, starting the binary, or probing a port.

## Proposed Design

Treat validation and execution as a capability separate from `write_file`.

Do not add an unrestricted language executable to `comm.go` and describe it as build-only validation. Allowlisting `go`, for example, permits materially different subcommands including `go build`, `go test`, `go run`, and tool installation workflows.

Prefer one of these approaches:

1. Native structured actions such as `build_project`, `test_project`, and `run_project`, each with a narrow schema and policy.
2. Subcommand-aware command validation that permits only the explicitly authorized toolchain operation for the current run.

The allowed operation should be captured on `AgentRun` so continuation segments cannot widen authorization. User feedback may add authorization explicitly, but the change should be recorded as a new requirement and evidence event.

## Trust Boundary

Generated source is agent-authored input. Compiling or testing it can execute arbitrary code through compiler hooks, generators, test initialization, build scripts, package managers, or the resulting binary.

All such execution currently inherits the recon process's filesystem, process, and network permissions. The command allowlist is not a sandbox. Documentation and API callers must not treat successful policy validation as isolation.

Minimum safeguards:

- require explicit user intent for each operation class;
- restrict the working directory to an approved project root;
- apply operation-specific timeouts;
- scope background processes to the owning run and clean them up at terminal states;
- avoid dependency installation unless separately requested;
- do not send credentials or secrets to generated programs;
- record the exact command, exit status, and truncated output as evidence;
- never claim validation when the toolchain is unavailable or the command was not observed successfully.

Sandboxing generated code would require a separate isolation design, such as a constrained container or VM. It is not provided by command validation alone.

## Prompt Rules

The agent protocol should state:

- Successful `write_file` observations complete ordinary file-generation goals.
- Never build, test, run, start, or probe generated code unless explicitly requested.
- Do not substitute one operation for another: building is not testing, and testing is not permission to start a service.
- If the requested toolchain or operation is unavailable, request clarification or return a precise capability error rather than pretending validation occurred.
- Cite successful command or structured-action observations for every validation claim.

Any heuristic that classifies user intent should be instrumented before it changes behavior. Ambiguous goals should fail closed by requesting clarification, not by executing generated code.

## Evidence and Verification

Validation observations must be recognized by `agentEvidenceEventIsValid` according to their semantics:

- a successful build proves compilation only;
- a successful test proves that the observed test command exited successfully;
- a successful background start proves only that a process started;
- a completed job or successful probe is needed to prove runtime behavior;
- failed validation remains valid evidence of failure but cannot support a positive success claim.

The critic should reject claims such as "tests pass" or "the server works" unless the cited evidence directly supports them.

## Tests

Add focused tests for:

- writing files without emitting any build, test, run, start, or probe action;
- explicit build authorization permitting compilation only;
- explicit test authorization permitting the selected test operation;
- build-only authorization rejecting test and run attempts;
- unavailable toolchains producing a clear capability result;
- operation timeouts and cancellation;
- cleanup of run-owned background processes;
- evidence semantics for successful and failed build, test, run, and probe operations;
- critic rejection when a positive validation claim lacks matching evidence;
- API integration where a write-only request stops after file creation;
- separate API integration where an explicitly authorized request writes, builds, starts, and probes a simple local application.

## Expected Result

A file-generation request writes the requested files and stops. Validation or execution occurs only when the user explicitly requests the corresponding operation, and every resulting claim is supported by direct evidence. The system never treats file creation as implicit permission to execute generated code.
