package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	agentMaxIterations  = 30
	agentRunTimeout     = 30 * time.Minute
	agentCommandTimeout = 2 * time.Minute
	agentMaxObservation = 32 * 1024
	agentContextTokens  = 4096
	agentSummaryTokens  = 768

	maxAgentListEntries    = 2000
	maxAgentListDepth      = 6
	maxAgentSearchMatches  = 200
	maxAgentSearchFileSize = 2 << 20
)

type AgentRunStatus string

const (
	agentRunning         AgentRunStatus = "running"
	agentWaitingApproval AgentRunStatus = "waiting_approval"
	agentWaitingInput    AgentRunStatus = "waiting_input"
	agentCompleted       AgentRunStatus = "completed"
	agentFailed          AgentRunStatus = "failed"
	agentCancelled       AgentRunStatus = "cancelled"
	agentLimitReached    AgentRunStatus = "limit_reached"
)

type AgentEvent struct {
	Sequence         int       `json:"sequence"`
	Kind             string    `json:"kind"`
	Action           string    `json:"action,omitempty"`
	Text             string    `json:"text,omitempty"`
	Command          string    `json:"command,omitempty"`
	Output           string    `json:"output,omitempty"`
	Success          bool      `json:"success,omitempty"`
	StepID           int       `json:"stepId,omitempty"`
	EvidenceEventIDs []int     `json:"evidenceEventIds,omitempty"`
	Thinking         string    `json:"thinking,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
}

type AgentRun struct {
	ID                  string          `json:"id"`
	SessionID           string          `json:"sessionId"`
	Goal                string          `json:"goal"`
	GoalSpec            AgentGoalSpec   `json:"goalSpec"`
	Status              AgentRunStatus  `json:"status"`
	Iteration           int             `json:"iteration"`
	MaxIterations       int             `json:"maxIterations"`
	StartedAt           time.Time       `json:"startedAt"`
	UpdatedAt           time.Time       `json:"updatedAt"`
	CompletedAt         time.Time       `json:"completedAt,omitempty"`
	BasePrompt          string          `json:"basePrompt"`
	BasePromptName      string          `json:"basePromptName"`
	FinalAnswer         string          `json:"finalAnswer,omitempty"`
	Error               string          `json:"error,omitempty"`
	Events              []AgentEvent    `json:"events"`
	ConsecutiveErrors   int             `json:"consecutiveErrors"`
	AvailableCommands   []string        `json:"availableCommands"`
	UnavailableCommands []string        `json:"unavailableCommands,omitempty"`
	WorkingDirectory    string          `json:"workingDirectory"`
	OperatingSystem     string          `json:"operatingSystem"`
	ContextSummary      string          `json:"contextSummary,omitempty"`
	CompactedThrough    int             `json:"compactedThrough,omitempty"`
	Plan                []AgentPlanStep `json:"plan,omitempty"`
	CandidateHistory    []string        `json:"candidateHistory,omitempty"`
	UserRequirements    []string        `json:"userRequirements,omitempty"`
	AttachmentNote      string          `json:"attachmentNote,omitempty"`
}

type AgentPlanStep struct {
	ID               int    `json:"id"`
	Description      string `json:"description"`
	Status           string `json:"status"`
	EvidenceEventIDs []int  `json:"evidenceEventIds,omitempty"`
}

type AgentVerification struct {
	Approved         bool   `json:"approved"`
	Feedback         string `json:"feedback"`
	EvidenceEventIDs []int  `json:"evidenceEventIds,omitempty"`
}

type AgentGoalSpec struct {
	Objective          string `json:"objective"`
	ExpectedOutput     string `json:"expectedOutput"`
	Constraints        string `json:"constraints,omitempty"`
	CompletionCriteria string `json:"completionCriteria"`
}

func newAgentRun(sessionID, goal, basePrompt, basePromptName string) *AgentRun {
	now := time.Now().UTC()
	available, unavailable := detectAgentCommands()
	workingDirectory, _ := os.Getwd()
	return &AgentRun{
		ID:                  fmt.Sprintf("run_%d", time.Now().UnixNano()),
		SessionID:           sessionID,
		Goal:                goal,
		GoalSpec:            normalizeAgentGoalSpec(AgentGoalSpec{Objective: goal}),
		Status:              agentRunning,
		MaxIterations:       agentMaxIterations,
		StartedAt:           now,
		UpdatedAt:           now,
		BasePrompt:          basePrompt,
		BasePromptName:      basePromptName,
		Events:              make([]AgentEvent, 0),
		AvailableCommands:   available,
		UnavailableCommands: unavailable,
		WorkingDirectory:    workingDirectory,
		OperatingSystem:     runtime.GOOS,
	}
}

func detectAgentCommands() ([]string, []string) {
	available := make([]string, 0, len(allowedCommands))
	unavailable := make([]string, 0)
	for _, command := range allowedCommandsList() {
		if _, err := exec.LookPath(command); err == nil {
			available = append(available, command)
		} else {
			unavailable = append(unavailable, command)
		}
	}
	return available, unavailable
}

func normalizeAgentGoalSpec(spec AgentGoalSpec) AgentGoalSpec {
	spec.Objective = strings.TrimSpace(spec.Objective)
	spec.ExpectedOutput = strings.TrimSpace(spec.ExpectedOutput)
	spec.Constraints = strings.TrimSpace(spec.Constraints)
	spec.CompletionCriteria = strings.TrimSpace(spec.CompletionCriteria)
	if spec.ExpectedOutput == "" {
		spec.ExpectedOutput = "A concise result that directly addresses the objective."
	}
	if spec.CompletionCriteria == "" {
		spec.CompletionCriteria = "The objective is fully addressed and claims based on tools are supported by successful observations."
	}
	return spec
}

func agentGoalDescription(run *AgentRun) string {
	spec := normalizeAgentGoalSpec(run.GoalSpec)
	description := fmt.Sprintf("OBJECTIVE:\n%s\n\nEXPECTED OUTPUT:\n%s\n\nCONSTRAINTS:\n%s\n\nCOMPLETION CRITERIA:\n%s", spec.Objective, spec.ExpectedOutput, emptyAgentValue(spec.Constraints), spec.CompletionCriteria)
	if run.AttachmentNote != "" {
		description += "\n\nATTACHMENTS:\n" + run.AttachmentNote
	}
	return description
}

func emptyAgentValue(value string) string {
	if value == "" {
		return "None specified."
	}
	return value
}

func (s *agentSession) appendAgentEvent(event AgentEvent) {
	s.mu.Lock()
	if s.AgentRun == nil {
		s.mu.Unlock()
		return
	}
	event.Sequence = len(s.AgentRun.Events) + 1
	event.CreatedAt = time.Now().UTC()
	s.AgentRun.Events = append(s.AgentRun.Events, event)
	s.AgentRun.UpdatedAt = event.CreatedAt
	sink := s.eventSink
	s.mu.Unlock()
	if sink != nil {
		sink(event)
	}
}

func (s *agentSession) agentSnapshot() *AgentRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AgentRun == nil {
		return nil
	}
	snapshot := *s.AgentRun
	snapshot.Events = append([]AgentEvent(nil), s.AgentRun.Events...)
	snapshot.CandidateHistory = append([]string(nil), s.AgentRun.CandidateHistory...)
	snapshot.UserRequirements = append([]string(nil), s.AgentRun.UserRequirements...)
	snapshot.Plan = clonePlanSteps(s.AgentRun.Plan)
	return &snapshot
}

func (s *agentSession) finishAgentRun(status AgentRunStatus, answer, errorText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AgentRun == nil {
		return
	}
	now := time.Now().UTC()
	s.AgentRun.Status = status
	s.AgentRun.FinalAnswer = answer
	s.AgentRun.Error = errorText
	s.AgentRun.UpdatedAt = now
	s.AgentRun.CompletedAt = now
	s.AgentMode = status == agentRunning || status == agentWaitingApproval || status == agentWaitingInput
}

func (s *agentSession) cancelAgentRun() {
	s.mu.Lock()
	cancel := s.agentCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func agentRunContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, agentRunTimeout)
}

func runAgent(ctx context.Context, sess *agentSession) {
	sess.appendAgentEvent(AgentEvent{Kind: "status", Text: "Agent run started"})
	defer func() {
		if err := sess.persist(); err != nil {
			logAgentError(sess.ID, err)
		}
	}()

	for {
		run := sess.agentSnapshot()
		if run == nil || run.Status != agentRunning {
			return
		}
		if run.Iteration >= run.MaxIterations {
			finishAgent(sess, agentLimitReached, "", "maximum iteration limit reached")
			return
		}
		select {
		case <-ctx.Done():
			status := agentCancelled
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				status = agentLimitReached
			}
			finishAgent(sess, status, "", ctx.Err().Error())
			return
		default:
		}

		sess.mu.Lock()
		sess.AgentRun.Iteration++
		sess.AgentRun.UpdatedAt = time.Now().UTC()
		sess.mu.Unlock()
		compactAgentContext(ctx, sess)

		messages, err := agentMessages(sess)
		if err != nil {
			finishAgent(sess, agentFailed, "", err.Error())
			return
		}
		response, err := callOllamaMessageWithRetry(ctx, "agent:"+run.ID, messages, true, sess.ThinkingEnabled)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !recordAgentFailure(sess, "model request failed: "+err.Error()) {
				return
			}
			continue
		}

		action, err := parseAgentAction(response.Content)
		if err != nil {
			sess.appendAgentEvent(AgentEvent{Kind: "invalid_action", Text: response.Content, Thinking: response.Thinking})
			if isTruncatedAgentJSON(err) {
				message := "Your previous response was cut off before the JSON object was fully closed (the answer was likely too long). Return the same action again, but keep the response short enough to complete: for a code deliverable, omit comments and blank lines, or split it into a smaller self-contained answer, and make sure the JSON object is fully closed."
				if !recordAgentRevision(sess, message, nil) {
					return
				}
				continue
			}
			if !recordAgentFailure(sess, err.Error()) {
				return
			}
			continue
		}
		if err := validateAgentActionState(sess, run, action); err != nil {
			sess.appendAgentEvent(AgentEvent{Kind: "invalid_action", Text: response.Content, Thinking: response.Thinking})
			if !recordAgentFailure(sess, err.Error()) {
				return
			}
			continue
		}
		if command, required := agentCommandRecoveryRequired(run.Events); required && action.Action != "run_command" {
			message := fmt.Sprintf("The latest command attempt failed: %q. Inspect its observation. Your next action must be run_command with a corrected or alternative command.", command)
			if !recordAgentRevision(sess, message, nil) {
				return
			}
			continue
		}
		if action.Action == "answer" {
			if goalRequiresCodeDeliverable(run) && !candidateHasCodeBlock(action.Text) {
				logAgentHeuristic(run.ID, "goalRequiresCodeDeliverable", "blocked candidate answer with no fenced code block")
				message := "The goal requires source code, but the candidate contains no non-empty fenced code block. Return the complete runnable code directly in the answer."
				sess.appendAgentEvent(AgentEvent{Kind: "candidate", Action: "answer", Text: action.Text, Thinking: response.Thinking})
				if !recordAgentRevision(sess, message, nil) {
					return
				}
				continue
			}
			if command := requiredGoalCommand(run); command != "" && !hasCommandObservation(run.Events, command) {
				logAgentHeuristic(run.ID, "requiredGoalCommand", fmt.Sprintf("blocked answer before a successful %q observation existed", command))
				message := fmt.Sprintf("goal requires executing %q before answering; return run_command with that command", command)
				sess.appendAgentEvent(AgentEvent{Kind: "invalid_action", Text: response.Content, Thinking: response.Thinking})
				if !recordAgentFailure(sess, message) {
					return
				}
				continue
			}
			verification, err := verifyAgentAnswer(ctx, sess, action.Text)
			if err != nil {
				if !recordAgentFailure(sess, "answer verification failed: "+err.Error()) {
					return
				}
				continue
			}
			if !verification.Approved {
				feedback := strings.TrimSpace(verification.Feedback)
				if feedback == "" {
					feedback = "Candidate answer did not satisfy the goal and completion criteria."
				}
				sess.appendAgentEvent(AgentEvent{Kind: "candidate", Action: "answer", Text: action.Text, Thinking: response.Thinking})
				if !recordAgentRevision(sess, feedback, verification.EvidenceEventIDs) {
					return
				}
				continue
			}
			action.EvidenceEventIDs = append([]int(nil), verification.EvidenceEventIDs...)
		}
		resetAgentFailures(sess)
		sess.appendAgentEvent(AgentEvent{Kind: "action", Action: action.Action, Text: action.Text, Command: action.Command, EvidenceEventIDs: action.EvidenceEventIDs, Thinking: response.Thinking})
		if err := sess.persist(); err != nil {
			finishAgent(sess, agentFailed, "", "could not persist action before execution: "+err.Error())
			return
		}

		if !executeAgentAction(ctx, sess, action) {
			return
		}
		if err := sess.persist(); err != nil {
			logAgentError(sess.ID, err)
		}
	}
}

func agentMessages(sess *agentSession) ([]ChatMessage, error) {
	run := sess.agentSnapshot()
	if run == nil {
		return nil, errors.New("agent run not found")
	}
	history := "Recent events are provided as conversation messages below."
	if run.ContextSummary != "" {
		history = "Earlier event summary:\n" + run.ContextSummary + "\n\n" + history
	}
	prompt, err := composeAgentPrompt(run.BasePrompt, agentGoalDescription(run), history)
	if err != nil {
		return nil, err
	}
	prompt += fmt.Sprintf("\n\nEXECUTION ENVIRONMENT:\nOS: %s\nWorking directory: %s\nCommands installed and allowed: %s\nAllowed but unavailable: %s\nCommands run with the same filesystem and network permissions as the recon process.", run.OperatingSystem, run.WorkingDirectory, strings.Join(run.AvailableCommands, ", "), emptyAgentValue(strings.Join(run.UnavailableCommands, ", ")))
	if revisionContext := agentRevisionContext(run); revisionContext != "" {
		prompt += "\n\nREVISION CONTEXT:\n" + revisionContext + "\nReturn a self-contained candidate containing the complete updated deliverable. Never claim that a previous response contains a requested change; include the changed result in this answer."
	}
	if len(run.Plan) > 0 {
		planJSON, _ := json.Marshal(run.Plan)
		prompt += "\n\nCURRENT PLAN:\n" + string(planJSON)
	}
	messages := []ChatMessage{{Role: "system", Content: prompt}}
	if command := requiredGoalCommand(run); command != "" && !hasCommandObservation(run.Events, command) {
		messages[0].Content += fmt.Sprintf("\n\nEXECUTION REQUIREMENT: The goal explicitly requires %q. You must return run_command using %q and inspect its real output before returning answer. A description, refusal, or hypothetical result is not completion.", command, command)
	}
	if command, required := agentCommandRecoveryRequired(run.Events); required {
		messages[0].Content += fmt.Sprintf("\n\nCOMMAND RECOVERY REQUIREMENT: The latest command attempt failed: %q. Your next action must be run_command with corrected syntax or an alternative command. Do not answer, update findings, change the plan, or request clarification before making that attempt.", command)
	}
	if reason, required := agentInvalidActionRecoveryRequired(run.Events); required {
		messages[0].Content += fmt.Sprintf("\n\nINVALID ACTION REQUIREMENT: Your previous response was rejected: %q. Return a corrected single JSON action from the allowed schemas that fixes this specific problem. Do not repeat the same invalid response.", reason)
	}
	if run.ContextSummary != "" {
		messages = append(messages, ChatMessage{Role: "user", Content: "Earlier run summary:\n" + run.ContextSummary})
	}
	for _, event := range selectAgentContextEvents(run.Events, run.CompactedThrough, agentContextTokens) {
		switch event.Kind {
		case "action", "invalid_action":
			if event.Kind == "invalid_action" {
				messages = append(messages, ChatMessage{Role: "assistant", Content: event.Text})
			} else {
				content, _ := json.Marshal(ToolResponse{Action: event.Action, Command: event.Command, Text: event.Text, StepID: event.StepID, EvidenceEventIDs: event.EvidenceEventIDs})
				messages = append(messages, ChatMessage{Role: "assistant", Content: string(content)})
			}
		case "observation", "error":
			messages = append(messages, ChatMessage{Role: "user", Content: "Tool observation:\n" + event.Text + "\n" + event.Output})
		case "feedback":
			messages = append(messages, ChatMessage{Role: "user", Content: "User feedback:\n" + event.Text})
		case "verification":
			messages = append(messages, ChatMessage{Role: "user", Content: "Independent critic feedback:\n" + event.Text})
		case "candidate":
			messages = append(messages, ChatMessage{Role: "assistant", Content: "Rejected candidate answer:\n" + event.Text})
		case "plan":
			messages = append(messages, ChatMessage{Role: "user", Content: "Plan update:\n" + event.Text})
		}
	}
	if len(messages) == 1 {
		instruction := "Begin working on the goal. Answer directly only if no tool is needed."
		if command := requiredGoalCommand(run); command != "" && !hasCommandObservation(run.Events, command) {
			instruction = fmt.Sprintf("Begin by executing the required %q command. Do not answer until you have inspected its output.", command)
		}
		messages = append(messages, ChatMessage{Role: "user", Content: instruction})
	} else {
		messages = append(messages, ChatMessage{Role: "user", Content: "Choose the next action. If the goal is complete, return answer."})
	}
	if images := agentImages(sess); len(images) > 0 {
		messages[len(messages)-1].Images = images
	}
	return messages, nil
}

func agentImages(sess *agentSession) []string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	images := make([]string, 0, len(sess.AttachedFiles))
	for _, attachment := range sess.AttachedFiles {
		if attachment != nil && attachment.Type == "image" {
			images = append(images, attachment.Content)
		}
	}
	return images
}

func requiredGoalCommand(run *AgentRun) string {
	fields := agentGoalFields(run.GoalSpec.Objective)
	if len(fields) == 0 {
		return ""
	}
	if allowedCommands[fields[0]] {
		return fields[0]
	}
	for index := 0; index+1 < len(fields); index++ {
		if (fields[index] == "run" || fields[index] == "execute") && allowedCommands[fields[index+1]] {
			return fields[index+1]
		}
	}
	return ""
}

// logAgentHeuristic records when a keyword-based goal-requirement heuristic
// (requiredGoalCommand, goalRequiresCodeDeliverable) actually changes the
// outcome, so real usage can be measured before deciding whether to refine or
// remove them — see AGENT.md #6. Purely observational: never affects behavior.
func logAgentHeuristic(runID, heuristic, detail string) {
	log.Printf("[agent-heuristic][%s] %s: %s", runID, heuristic, detail)
}

func agentGoalFields(goal string) []string {
	return strings.FieldsFunc(strings.ToLower(goal), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' && r != '-'
	})
}

func hasCommandObservation(events []AgentEvent, requiredCommand string) bool {
	for _, event := range events {
		if event.Kind != "observation" || event.Action != "run_command" || !event.Success {
			continue
		}
		fields := strings.Fields(event.Command)
		if len(fields) > 0 && fields[0] == requiredCommand {
			return true
		}
	}
	return false
}

func goalRequiresCodeDeliverable(run *AgentRun) bool {
	text := strings.ToLower(strings.Join([]string{run.GoalSpec.Objective, run.GoalSpec.ExpectedOutput, run.GoalSpec.CompletionCriteria}, " "))
	creationVerb := false
	for _, verb := range []string{"write", "develop", "build", "implement", "create", "generate"} {
		if strings.Contains(text, verb) {
			creationVerb = true
			break
		}
	}
	if !creationVerb {
		return false
	}
	for _, artifact := range []string{"code", "app", "application", "program", "server"} {
		if strings.Contains(text, artifact) {
			return true
		}
	}
	return false
}

func candidateHasCodeBlock(candidate string) bool {
	start := strings.Index(candidate, "```")
	if start < 0 {
		return false
	}
	contentStart := strings.Index(candidate[start+3:], "\n")
	if contentStart < 0 {
		return false
	}
	contentStart += start + 4
	end := strings.Index(candidate[contentStart:], "```")
	return end > 0 && strings.TrimSpace(candidate[contentStart:contentStart+end]) != ""
}

func latestAgentCommandFailure(events []AgentEvent) (string, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Kind == "observation" && event.Action == "run_command" {
			return event.Command, !event.Success
		}
	}
	return "", false
}

func agentCommandRecoveryRequired(events []AgentEvent) (string, bool) {
	command, failed := latestAgentCommandFailure(events)
	return command, failed && consecutiveAgentCommandFailures(events) < 2
}

// agentInvalidActionRecoveryRequired detects a parse/validation rejection from the
// immediately preceding iteration so the model gets one explicit corrective nudge.
func agentInvalidActionRecoveryRequired(events []AgentEvent) (string, bool) {
	if len(events) < 2 {
		return "", false
	}
	last := events[len(events)-1]
	prior := events[len(events)-2]
	if last.Kind == "error" && prior.Kind == "invalid_action" {
		return last.Text, true
	}
	return "", false
}

func consecutiveAgentCommandFailures(events []AgentEvent) int {
	failures := 0
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Kind != "observation" || event.Action != "run_command" {
			continue
		}
		if event.Success {
			break
		}
		failures++
	}
	return failures
}

func validateAgentActionState(sess *agentSession, run *AgentRun, action ToolResponse) error {
	if action.Action == "check_job" || action.Action == "get_job" || action.Action == "cancel_job" {
		job, ok := jobStore.get(action.JobID)
		if !ok || job.SessionID != sess.ID || job.RunID != run.ID {
			return fmt.Errorf("job %q was not created by this run; start a background job first and use its returned jobId", action.JobID)
		}
	}
	if action.Action == "complete_step" {
		if action.StepID > len(run.Plan) {
			return fmt.Errorf("plan step %d does not exist", action.StepID)
		}
		for _, eventID := range action.EvidenceEventIDs {
			if eventID <= 0 || eventID > len(run.Events) {
				return fmt.Errorf("evidence event %d does not exist", eventID)
			}
			if event := run.Events[eventID-1]; !agentEvidenceEventIsValid(event) {
				return fmt.Errorf("evidence event %d (%s/%s) cannot prove this step; cite a successful observation, a terminal job snapshot, or user feedback", eventID, event.Kind, event.Action)
			}
		}
	}
	return nil
}

func agentEventHistory(events []AgentEvent) string {
	if len(events) == 0 {
		return "No previous attempts yet."
	}
	var history strings.Builder
	for _, event := range events {
		fmt.Fprintf(&history, "%d. %s %s %s\n", event.Sequence, event.Kind, event.Action, truncateAgentText(event.Text+event.Output))
	}
	return history.String()
}

func estimateAgentTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

func agentEventText(event AgentEvent) string {
	data, _ := json.Marshal(event)
	return string(data)
}

func selectAgentContextEvents(events []AgentEvent, compactedThrough, tokenBudget int) []AgentEvent {
	selectedStart := len(events)
	used := 0
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Sequence <= compactedThrough {
			break
		}
		tokens := estimateAgentTokens(agentEventText(events[index]))
		if selectedStart < len(events) && used+tokens > tokenBudget {
			break
		}
		selectedStart = index
		used += tokens
	}
	return append([]AgentEvent(nil), events[selectedStart:]...)
}

func compactAgentContext(ctx context.Context, sess *agentSession) {
	run := sess.agentSnapshot()
	if run == nil {
		return
	}
	pending := make([]AgentEvent, 0)
	totalTokens := 0
	for _, event := range run.Events {
		if event.Sequence <= run.CompactedThrough {
			continue
		}
		pending = append(pending, event)
		totalTokens += estimateAgentTokens(agentEventText(event))
	}
	if len(pending) < 2 || totalTokens <= agentContextTokens {
		return
	}
	keep := selectAgentContextEvents(pending, 0, agentContextTokens/2)
	compactCount := len(pending) - len(keep)
	if compactCount <= 0 {
		return
	}
	toCompact := pending[:compactCount]
	summaryInput := truncateAgentText(agentEventHistory(toCompact))
	summaryMessages := []ChatMessage{
		{Role: "system", Content: fmt.Sprintf("Summarize agent-agent history in at most %d tokens. Preserve verified facts, failed approaches, unresolved questions, user constraints, and job IDs. Do not invent information.", agentSummaryTokens)},
		{Role: "user", Content: "Existing summary:\n" + emptyAgentValue(run.ContextSummary) + "\n\nEvents to compact:\n" + summaryInput},
	}
	response, err := callOllamaMessageWithRetry(ctx, "agent-summary:"+run.ID, summaryMessages, false, false)
	summary := strings.TrimSpace(response.Content)
	if err != nil || summary == "" {
		summary = mergeAgentSummaryFallback(run.ContextSummary, summaryInput)
	}
	sess.mu.Lock()
	if sess.AgentRun != nil && sess.AgentRun.ID == run.ID {
		sess.AgentRun.ContextSummary = summary
		sess.AgentRun.CompactedThrough = toCompact[len(toCompact)-1].Sequence
		sess.AgentRun.UpdatedAt = time.Now().UTC()
	}
	sess.mu.Unlock()
}

// mergeAgentSummaryFallback combines a prior context summary with newly compacted
// event text when the summarization model call fails, keeping the newest facts
// (summaryInput) intact and truncating from the front of the older summary instead
// of the back, so CompactedThrough never advances past facts that were dropped.
func mergeAgentSummaryFallback(previous, newest string) string {
	newest = truncateAgentText(newest)
	if len(newest) >= agentMaxObservation || previous == "" {
		return newest
	}
	budget := agentMaxObservation - len(newest) - len("\n")
	if len(previous) > budget {
		previous = previous[len(previous)-budget:]
	}
	return previous + "\n" + newest
}

// agentPlanEvidenceEventIDs returns the deduped, sorted set of evidence event IDs
// cited by completed plan steps, regardless of context-compaction state.
func agentPlanEvidenceEventIDs(plan []AgentPlanStep) []int {
	seen := make(map[int]bool)
	var ids []int
	for _, step := range plan {
		if step.Status != "completed" {
			continue
		}
		for _, id := range step.EvidenceEventIDs {
			if id > 0 && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	sort.Ints(ids)
	return ids
}

// agentEvidenceLedger looks up events cited by completed plan steps directly from
// run.Events (never truncated, only excluded from the recent-context window), so
// the critic can still inspect evidence that compaction folded into ContextSummary.
func agentEvidenceLedger(run *AgentRun, windowed []AgentEvent) []AgentEvent {
	inWindow := make(map[int]bool, len(windowed))
	for _, event := range windowed {
		inWindow[event.Sequence] = true
	}
	var ledger []AgentEvent
	for _, id := range agentPlanEvidenceEventIDs(run.Plan) {
		if id > len(run.Events) || inWindow[id] {
			continue
		}
		ledger = append(ledger, run.Events[id-1])
	}
	return ledger
}

func verifyAgentAnswer(ctx context.Context, sess *agentSession, candidate string) (AgentVerification, error) {
	run := sess.agentSnapshot()
	if run == nil {
		return AgentVerification{}, errors.New("agent run not found")
	}
	windowed := selectAgentContextEvents(run.Events, run.CompactedThrough, agentContextTokens)
	evidence := agentCriticEvidence(windowed)
	evidence = append(evidence, agentEvidenceLedger(run, windowed)...)
	evidenceJSON, _ := json.Marshal(evidence)
	planJSON, _ := json.Marshal(run.Plan)
	revisionContext := agentRevisionContext(run)
	userContent := agentGoalDescription(run) + "\n\nREVISION CONTEXT:\n" + revisionContext + "\n\nPLAN:\n" + string(planJSON)
	if run.ContextSummary != "" {
		userContent += "\n\nEARLIER RUN SUMMARY:\n" + run.ContextSummary
	}
	userContent += "\n\nCANDIDATE ANSWER:\n" + candidate + "\n\nRECORDED EVENTS:\n" + string(evidenceJSON)
	messages := []ChatMessage{
		{Role: "system", Content: "You are an independent critic. Verify the exact candidate agent-agent answer against the objective, expected output, constraints, completion criteria, plan, user feedback, and recorded events. Approve only if the candidate itself contains the complete requested deliverable. Never assume promised, described, or omitted content exists; a preamble without its claimed output is incomplete. Return exactly one JSON object: {\"approved\":true|false,\"feedback\":\"specific reason or empty\",\"evidenceEventIds\":[1,2]}. Cite only event IDs that directly support the answer. Knowledge-only answers may use an empty evidence list."},
		{Role: "user", Content: userContent, Images: agentImages(sess)},
	}
	response, err := callOllamaMessageWithRetry(ctx, "agent-critic:"+run.ID, messages, true, false)
	if err != nil {
		return AgentVerification{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(response.Content)))
	decoder.DisallowUnknownFields()
	var verification AgentVerification
	if err := decoder.Decode(&verification); err != nil {
		return AgentVerification{}, fmt.Errorf("invalid critic JSON: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return AgentVerification{}, errors.New("critic response must contain exactly one JSON object")
	}
	// Plan step status is worker bookkeeping, not a completion contract: an
	// evidence-backed critic approval must not be downgraded solely because a
	// model-created plan step is still pending. The critic already receives
	// the plan and can reject the candidate itself if real work is missing.
	for _, eventID := range verification.EvidenceEventIDs {
		if eventID <= 0 || eventID > len(run.Events) {
			return AgentVerification{}, fmt.Errorf("critic cited nonexistent event %d", eventID)
		}
	}
	if verification.Approved {
		for _, eventID := range verification.EvidenceEventIDs {
			if event := run.Events[eventID-1]; !agentEvidenceEventIsValid(event) {
				verification.Approved = false
				verification.Feedback = fmt.Sprintf("cited event %d (%s/%s) cannot prove the answer; cite a successful observation, a terminal job snapshot, or user feedback instead", eventID, event.Kind, event.Action)
				break
			}
		}
	}
	if command := requiredGoalCommand(run); verification.Approved && command != "" && !evidenceSupportsCommand(run.Events, verification.EvidenceEventIDs, command) {
		logAgentHeuristic(run.ID, "requiredGoalCommand", fmt.Sprintf("critic approved but cited no successful %q observation; downgraded to revision", command))
		verification.Approved = false
		verification.Feedback = fmt.Sprintf("candidate does not cite a successful %q observation", command)
	}
	return verification, nil
}

func agentCriticEvidence(events []AgentEvent) []AgentEvent {
	evidence := make([]AgentEvent, 0, len(events))
	for _, event := range events {
		switch event.Kind {
		case "observation", "feedback", "plan":
			evidence = append(evidence, event)
		}
	}
	return evidence
}

func agentRevisionContext(run *AgentRun) string {
	if len(run.CandidateHistory) == 0 && len(run.UserRequirements) == 0 {
		return ""
	}
	var contextText strings.Builder
	if len(run.CandidateHistory) > 0 {
		contextText.WriteString("Previously shown candidate answers, oldest to newest:\n")
		start := len(run.CandidateHistory) - 4
		if start < 0 {
			start = 0
		}
		for index, candidate := range run.CandidateHistory[start:] {
			fmt.Fprintf(&contextText, "\nCandidate %d:\n%s\n", start+index+1, truncateAgentText(candidate))
		}
	}
	if len(run.UserRequirements) > 0 {
		contextText.WriteString("\nUser-requested changes, oldest to newest:\n")
		for index, requirement := range run.UserRequirements {
			fmt.Fprintf(&contextText, "%d. %s\n", index+1, requirement)
		}
	}
	return strings.TrimSpace(contextText.String())
}

func evidenceSupportsCommand(events []AgentEvent, evidenceIDs []int, command string) bool {
	for _, eventID := range evidenceIDs {
		event := events[eventID-1]
		fields := strings.Fields(event.Command)
		if event.Kind == "observation" && event.Action == "run_command" && event.Success && len(fields) > 0 && fields[0] == command {
			return true
		}
	}
	return false
}

var agentJobObservationStatusPattern = regexp.MustCompile(`status: (\w+)`)

// agentJobObservationStatus extracts the job status embedded in a check_job/get_job
// observation's Text (see the "job %s status: %s" format in executeAgentAction).
func agentJobObservationStatus(text string) (string, bool) {
	match := agentJobObservationStatusPattern.FindStringSubmatch(text)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// agentEvidenceEventIsValid applies a deterministic, per-action evidence policy so
// citing an event can only prove what that event actually recorded. Semantic
// relevance to a specific claim (does this evidence actually support THIS answer
// or step) is left to the critic and to plan review; this only rejects citations
// that are structurally impossible proof (a failed observation, a rejected draft,
// an error, or the model's own stated intent).
func agentEvidenceEventIsValid(event AgentEvent) bool {
	switch event.Kind {
	case "observation":
		switch event.Action {
		case "run_command", "list_directory", "search_files":
			// A successful foreground/native observation can prove its own result;
			// a failed one proves nothing positive.
			return event.Success
		case "run_command_bg":
			// A background start can prove only that a job started, not its outcome.
			return true
		case "check_job", "get_job":
			// A job snapshot can prove a final result only once the job reached a
			// terminal state; "running" proves nothing about the eventual outcome.
			status, ok := agentJobObservationStatus(event.Text)
			return ok && status != "running"
		case "cancel_job", "list_jobs", "update_findings":
			return true
		default:
			return false
		}
	case "feedback":
		// User feedback can prove a stated requirement, not command execution.
		return true
	case "plan":
		return true
	default:
		// "action" (stated intent), "invalid_action", "candidate", "answer",
		// "verification", "clarification", "status", and "error" events record
		// intent, rejection, or failure — never structurally valid proof of a claim.
		return false
	}
}

// isTruncatedAgentJSON reports whether a parseAgentAction failure looks like
// the model's response was cut off mid-JSON (e.g. a long code answer hitting
// its output limit) rather than a genuinely malformed or wrong-schema
// response. Truncation is recoverable by asking for a shorter complete
// answer, so callers should not spend a strike of the 3-failure budget on it.
func isTruncatedAgentJSON(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF)
}

func parseAgentAction(raw string) (ToolResponse, error) {
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	decoder.DisallowUnknownFields()
	var action ToolResponse
	if err := decoder.Decode(&action); err != nil {
		return ToolResponse{}, fmt.Errorf("invalid agent action JSON: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return ToolResponse{}, errors.New("agent response must contain exactly one JSON object")
	}
	switch action.Action {
	case "answer", "update_findings", "request_clarification":
		if strings.TrimSpace(action.Text) == "" {
			return ToolResponse{}, fmt.Errorf("action %q requires text", action.Action)
		}
	case "run_command", "run_command_bg":
		if err := validateAgentCommand(action.Command); err != nil {
			return ToolResponse{}, err
		}
	case "check_job", "get_job", "cancel_job":
		if strings.TrimSpace(action.JobID) == "" {
			return ToolResponse{}, fmt.Errorf("action %q requires jobId", action.Action)
		}
	case "list_jobs":
	case "list_directory":
		if strings.TrimSpace(action.Path) == "" {
			return ToolResponse{}, errors.New("action list_directory requires path")
		}
	case "search_files":
		if strings.TrimSpace(action.Path) == "" || strings.TrimSpace(action.Pattern) == "" {
			return ToolResponse{}, errors.New("action search_files requires path and pattern")
		}
		if _, err := regexp.Compile(action.Pattern); err != nil {
			return ToolResponse{}, fmt.Errorf("invalid search pattern: %w", err)
		}
	case "create_plan":
		if len(action.Steps) == 0 {
			return ToolResponse{}, errors.New("action create_plan requires at least one step")
		}
		for _, step := range action.Steps {
			if strings.TrimSpace(step) == "" {
				return ToolResponse{}, errors.New("plan steps must not be empty")
			}
		}
	case "complete_step":
		if action.StepID <= 0 {
			return ToolResponse{}, errors.New("action complete_step requires a positive stepId")
		}
	default:
		return ToolResponse{}, fmt.Errorf("unsupported agent action %q", action.Action)
	}
	if err := validateAgentActionFieldScope(action); err != nil {
		return ToolResponse{}, err
	}
	return action, nil
}

// agentActionAllowedFields maps each action to the ToolResponse fields it may
// carry (beyond "action" itself), matching the field usage in parseAgentAction
// and executeAgentAction. Anything else must be left at its zero value.
var agentActionAllowedFields = map[string]map[string]bool{
	"answer":                {"text": true},
	"update_findings":       {"text": true},
	"request_clarification": {"text": true},
	"run_command":           {"command": true},
	"run_command_bg":        {"command": true},
	"check_job":             {"jobId": true},
	"get_job":               {"jobId": true},
	"cancel_job":            {"jobId": true},
	"list_jobs":             {},
	"list_directory":        {"path": true, "maxDepth": true},
	"search_files":          {"path": true, "pattern": true},
	"create_plan":           {"steps": true},
	"complete_step":         {"stepId": true, "evidenceEventIds": true},
}

// validateAgentActionFieldScope rejects a response whose action carries a
// non-zero field belonging to a different action's schema (e.g. an "answer"
// with a "command", or "list_jobs" with "text"). Unknown JSON keys are already
// rejected by DisallowUnknownFields; this closes the remaining gap where
// ToolResponse's shared struct otherwise accepts and silently ignores fields
// that don't belong to the selected action, which can conceal model confusion.
func validateAgentActionFieldScope(action ToolResponse) error {
	allowed := agentActionAllowedFields[action.Action]
	var stray []string
	if action.Command != "" && !allowed["command"] {
		stray = append(stray, "command")
	}
	if action.Path != "" && !allowed["path"] {
		stray = append(stray, "path")
	}
	if action.Pattern != "" && !allowed["pattern"] {
		stray = append(stray, "pattern")
	}
	if action.MaxDepth != 0 && !allowed["maxDepth"] {
		stray = append(stray, "maxDepth")
	}
	if action.Text != "" && !allowed["text"] {
		stray = append(stray, "text")
	}
	if action.JobID != "" && !allowed["jobId"] {
		stray = append(stray, "jobId")
	}
	if len(action.Steps) > 0 && !allowed["steps"] {
		stray = append(stray, "steps")
	}
	if action.StepID != 0 && !allowed["stepId"] {
		stray = append(stray, "stepId")
	}
	if len(action.EvidenceEventIDs) > 0 && !allowed["evidenceEventIds"] {
		stray = append(stray, "evidenceEventIds")
	}
	if len(stray) == 0 {
		return nil
	}
	log.Printf("[agent-schema] action %q carried out-of-scope field(s): %s", action.Action, strings.Join(stray, ", "))
	return fmt.Errorf("action %q must not include field(s): %s", action.Action, strings.Join(stray, ", "))
}

func validateAgentCommand(command string) error {
	command = sanitizeCommand(command)
	if command == "" || strings.ContainsAny(command, "\n\r") {
		return errors.New("command must be a single physical line; heredocs and multi-line scripts are not supported. To deliver file or source content, use the answer action with the complete content in a fenced code block")
	}
	if strings.Contains(command, "$(") || strings.Contains(command, "`") {
		return errors.New("command substitution is not allowed")
	}
	for _, segment := range strings.FieldsFunc(command, func(r rune) bool { return r == '|' || r == ';' || r == '&' }) {
		fields := strings.Fields(strings.TrimSpace(segment))
		if len(fields) == 0 {
			continue
		}
		if strings.Contains(fields[0], "/") {
			commandName := filepath.Base(fields[0])
			if allowedCommands[commandName] {
				return fmt.Errorf("command %q must use the bare allowlisted name %q without a path", fields[0], commandName)
			}
		}
		if !allowedCommands[fields[0]] {
			return fmt.Errorf("command %q is not allowed", fields[0])
		}
	}
	return validateShAndXargsInvocations(command)
}

// tokenizeShellWords splits a command string into shell words, honoring single
// and double quotes (so a quoted argument containing spaces or operators stays
// one word) and backslash escapes outside single quotes. It is intentionally
// not a full POSIX shell parser (no variable expansion, command substitution —
// already rejected above — or here-docs); it exists only to find argument
// boundaries reliably enough to re-validate what "sh"/"xargs" invoke below.
func tokenizeShellWords(command string) ([]string, error) {
	var words []string
	var current strings.Builder
	hasCurrent := false
	runes := []rune(command)
	index := 0
	for index < len(runes) {
		switch r := runes[index]; r {
		case '\'':
			hasCurrent = true
			index++
			start := index
			for index < len(runes) && runes[index] != '\'' {
				index++
			}
			if index >= len(runes) {
				return nil, errors.New("unterminated single quote")
			}
			current.WriteString(string(runes[start:index]))
			index++
		case '"':
			hasCurrent = true
			index++
			for index < len(runes) && runes[index] != '"' {
				if runes[index] == '\\' && index+1 < len(runes) && (runes[index+1] == '"' || runes[index+1] == '\\') {
					index++
				}
				current.WriteRune(runes[index])
				index++
			}
			if index >= len(runes) {
				return nil, errors.New("unterminated double quote")
			}
			index++
		case '\\':
			if index+1 < len(runes) {
				current.WriteRune(runes[index+1])
				hasCurrent = true
				index += 2
			} else {
				index++
			}
		case ' ', '\t':
			if hasCurrent {
				words = append(words, current.String())
				current.Reset()
				hasCurrent = false
			}
			index++
		default:
			current.WriteRune(r)
			hasCurrent = true
			index++
		}
	}
	if hasCurrent {
		words = append(words, current.String())
	}
	return words, nil
}

// shScriptArgument returns the script text following "sh -c", the only "sh"
// invocation this validator can prove safe; any other usage (no -c, or -c
// combined with other short flags) is rejected since its effect can't be
// re-validated against the allowlist.
func shScriptArgument(rest []string) (string, bool) {
	if len(rest) < 2 || rest[0] != "-c" {
		return "", false
	}
	return rest[1], true
}

// xargsValueFlags lists xargs options that consume the next token as their
// value when given as a separate argument (e.g. "-I {}" rather than "-I{}").
var xargsValueFlags = map[string]bool{
	"-I": true, "-i": true, "-n": true, "-P": true, "-L": true,
	"-l": true, "-s": true, "-d": true, "-a": true, "-E": true, "-e": true,
}

// xargsCommandOffset finds the index within rest of the first non-option
// token — the command xargs will actually invoke — skipping xargs' own flags
// (and, for flags in xargsValueFlags, their separate value argument too).
func xargsCommandOffset(rest []string) (int, bool) {
	for index := 0; index < len(rest); index++ {
		if strings.HasPrefix(rest[index], "-") {
			if xargsValueFlags[rest[index]] {
				index++
			}
			continue
		}
		return index, true
	}
	return 0, false
}

// validateShAndXargsInvocations re-validates whatever "sh -c" or "xargs" would
// actually run against the same command allowlist. Both are themselves
// allowlisted (needed for legitimate multi-command workflows), but without
// this check either could run anything unchecked, making the allowlist not a
// real boundary. Scans every occurrence in the command, not just the first, and
// re-checks nested forms (e.g. "xargs sh -c \"...\"") the same way as a
// top-level command.
func validateShAndXargsInvocations(command string) error {
	words, err := tokenizeShellWords(command)
	if err != nil {
		return fmt.Errorf("could not parse command for sh/xargs validation: %w", err)
	}
	return validateShAndXargsWords(words)
}

func validateShAndXargsWords(words []string) error {
	for index := 0; index < len(words); index++ {
		switch words[index] {
		case "sh":
			script, ok := shScriptArgument(words[index+1:])
			if !ok {
				return errors.New(`"sh" is only allowed as sh -c "<command>"`)
			}
			if err := validateAgentCommand(script); err != nil {
				return fmt.Errorf("sh -c script rejected: %w", err)
			}
			index += 2 // skip "-c" and the script argument just validated
		case "xargs":
			rest := words[index+1:]
			offset, ok := xargsCommandOffset(rest)
			if !ok {
				return errors.New(`"xargs" must be followed by an allowlisted command`)
			}
			target := rest[offset]
			if !allowedCommands[target] {
				return fmt.Errorf("xargs target command %q is not allowed", target)
			}
			// Land on the target token itself next iteration, so a nested
			// "sh"/"xargs" target gets the same re-validation as a top-level one.
			index += offset
		}
	}
	return nil
}

func executeAgentAction(ctx context.Context, sess *agentSession, action ToolResponse) bool {
	switch action.Action {
	case "answer":
		sess.appendAgentEvent(AgentEvent{Kind: "answer", Action: action.Action, Text: action.Text, EvidenceEventIDs: action.EvidenceEventIDs})
		sess.appendAgentMessage(ChatMessage{Role: "assistant", Content: action.Text})
		sess.mu.Lock()
		sess.AgentRun.CandidateHistory = append(sess.AgentRun.CandidateHistory, action.Text)
		sess.AwaitingDecision = true
		sess.AgentRun.Status = agentWaitingApproval
		sess.AgentRun.FinalAnswer = action.Text
		sess.AgentRun.UpdatedAt = time.Now().UTC()
		sess.mu.Unlock()
		return false
	case "request_clarification":
		sess.appendAgentEvent(AgentEvent{Kind: "clarification", Action: action.Action, Text: action.Text})
		sess.mu.Lock()
		sess.AwaitingDecision = true
		sess.AgentRun.Status = agentWaitingInput
		sess.AgentRun.UpdatedAt = time.Now().UTC()
		sess.mu.Unlock()
		return false
	case "update_findings":
		sess.mu.Lock()
		sess.PartialFindings = action.Text
		sess.mu.Unlock()
		sess.appendAgentEvent(AgentEvent{Kind: "observation", Action: action.Action, Text: "Findings saved: " + action.Text})
		return true
	case "create_plan":
		plan := make([]AgentPlanStep, 0, len(action.Steps))
		for index, description := range action.Steps {
			plan = append(plan, AgentPlanStep{ID: index + 1, Description: strings.TrimSpace(description), Status: "pending"})
		}
		sess.mu.Lock()
		sess.AgentRun.Plan = plan
		sess.mu.Unlock()
		sess.appendAgentEvent(AgentEvent{Kind: "plan", Action: action.Action, Text: strings.Join(action.Steps, "\n")})
		return true
	case "complete_step":
		sess.mu.Lock()
		step := &sess.AgentRun.Plan[action.StepID-1]
		step.Status = "completed"
		step.EvidenceEventIDs = append([]int(nil), action.EvidenceEventIDs...)
		sess.mu.Unlock()
		sess.appendAgentEvent(AgentEvent{Kind: "plan", Action: action.Action, StepID: action.StepID, EvidenceEventIDs: action.EvidenceEventIDs, Text: fmt.Sprintf("Plan step %d completed", action.StepID)})
		return true
	case "run_command":
		commandCtx, cancel := context.WithTimeout(ctx, agentCommandTimeout)
		output, success := runAgentCommand(commandCtx, action.Command)
		cancel()
		sess.mu.Lock()
		sess.CommandCount++
		sess.Stats.recordCommand(action.Command)
		sess.mu.Unlock()
		sess.appendAgentEvent(AgentEvent{Kind: "observation", Action: action.Action, Command: action.Command, Output: truncateAgentText(output), Success: success})
		sess.appendAgentMessage(ChatMessage{Role: "assistant", Content: fmt.Sprintf("[Running]: %s\n[Command output]:\n%s", action.Command, output)})
		return true
	case "run_command_bg":
		run := sess.agentSnapshot()
		if run == nil {
			sess.appendAgentEvent(AgentEvent{Kind: "error", Action: action.Action, Text: "agent run not found"})
			return true
		}
		job, err := executeBackgroundCommand(sess.ID, run.ID, action.Command)
		if err != nil {
			sess.appendAgentEvent(AgentEvent{Kind: "error", Action: action.Action, Text: err.Error()})
			return true
		}
		sess.appendAgentEvent(AgentEvent{Kind: "observation", Action: action.Action, Text: fmt.Sprintf("job %s started", job.ID)})
		return true
	case "list_directory":
		output, success := listAgentDirectory(action.Path, action.MaxDepth)
		sess.appendAgentEvent(AgentEvent{Kind: "observation", Action: action.Action, Text: fmt.Sprintf("list_directory path=%q maxDepth=%d", action.Path, action.MaxDepth), Output: truncateAgentText(output), Success: success})
		return true
	case "search_files":
		output, success := searchAgentFiles(action.Path, action.Pattern)
		sess.appendAgentEvent(AgentEvent{Kind: "observation", Action: action.Action, Text: fmt.Sprintf("search_files path=%q pattern=%q", action.Path, action.Pattern), Output: truncateAgentText(output), Success: success})
		return true
	case "check_job", "get_job":
		job, ok := jobStore.get(action.JobID)
		if !ok || job.SessionID != sess.ID {
			sess.appendAgentEvent(AgentEvent{Kind: "error", Action: action.Action, Text: "job not found"})
			return true
		}
		output := job.Output
		if action.Action == "check_job" && len(output) > 1000 {
			output = output[:1000] + "\n... (truncated)"
		}
		sess.appendAgentEvent(AgentEvent{Kind: "observation", Action: action.Action, Text: fmt.Sprintf("job %s status: %s\n", job.ID, job.Status), Output: truncateAgentText(output)})
		return true
	case "cancel_job":
		jobStore.cancel(action.JobID, sess.ID)
		sess.appendAgentEvent(AgentEvent{Kind: "observation", Action: action.Action, Text: "job cancellation processed"})
		return true
	case "list_jobs":
		jobs := jobStore.listBySession(sess.ID)
		var text strings.Builder
		for _, job := range jobs {
			fmt.Fprintf(&text, "%s: %s [%s]\n", job.ID, job.Command, job.Status)
		}
		if text.Len() == 0 {
			text.WriteString("No jobs")
		}
		sess.appendAgentEvent(AgentEvent{Kind: "observation", Action: action.Action, Text: text.String()})
		return true
	}
	return true
}

func runAgentCommand(ctx context.Context, command string) (string, bool) {
	command = sanitizeCommand(command)
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\n[stderr]\n" + stderr.String()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		output += "\nCommand timed out."
	} else if err != nil {
		output += "\nError: " + err.Error()
	}
	if strings.TrimSpace(output) == "" {
		output = "(no output)"
	}
	return output, err == nil && ctx.Err() == nil
}

// listAgentDirectory produces a deterministic, depth-limited directory listing
// without requiring the model to compose ls/find/tree syntax.
func listAgentDirectory(rootPath string, maxDepth int) (string, bool) {
	rootPath = strings.TrimSpace(rootPath)
	if rootPath == "" {
		return "path is required", false
	}
	absoluteRoot, err := filepath.Abs(rootPath)
	if err != nil {
		return err.Error(), false
	}
	info, err := os.Stat(absoluteRoot)
	if err != nil {
		return err.Error(), false
	}
	if !info.IsDir() {
		return fmt.Sprintf("%s is not a directory", absoluteRoot), false
	}
	if maxDepth <= 0 || maxDepth > maxAgentListDepth {
		maxDepth = maxAgentListDepth
	}

	var lines []string
	truncated := false
	walkErr := filepath.WalkDir(absoluteRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		relative, relErr := filepath.Rel(absoluteRoot, path)
		if relErr != nil || relative == "." {
			return nil
		}
		depth := len(strings.Split(relative, string(filepath.Separator)))
		if depth > maxDepth {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if len(lines) >= maxAgentListEntries {
			truncated = true
			return fs.SkipAll
		}
		kind := "file"
		var size int64
		if entry.IsDir() {
			kind = "dir"
		} else if fileInfo, statErr := entry.Info(); statErr == nil {
			size = fileInfo.Size()
		}
		lines = append(lines, fmt.Sprintf("%s\t%s\t%d", kind, relative, size))
		return nil
	})
	if walkErr != nil {
		return walkErr.Error(), false
	}
	if len(lines) == 0 {
		return fmt.Sprintf("%s is empty", absoluteRoot), true
	}
	sort.Strings(lines)
	header := fmt.Sprintf("%d entries under %s (maxDepth=%d)", len(lines), absoluteRoot, maxDepth)
	if truncated {
		header += fmt.Sprintf(", truncated at %d entries", maxAgentListEntries)
	}
	return header + "\n" + strings.Join(lines, "\n"), true
}

// searchAgentFiles runs a Go-native regex search over text files, avoiding
// shell grep syntax entirely.
func searchAgentFiles(rootPath, pattern string) (string, bool) {
	rootPath = strings.TrimSpace(rootPath)
	if rootPath == "" {
		return "path is required", false
	}
	regex, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Sprintf("invalid pattern: %v", err), false
	}
	absoluteRoot, err := filepath.Abs(rootPath)
	if err != nil {
		return err.Error(), false
	}
	info, err := os.Stat(absoluteRoot)
	if err != nil {
		return err.Error(), false
	}

	var matches []string
	truncated := false
	scanFile := func(path string) bool {
		fileInfo, statErr := os.Stat(path)
		if statErr != nil || fileInfo.Size() > maxAgentSearchFileSize {
			return true
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil || !looksLikeText(data) {
			return true
		}
		relative, relErr := filepath.Rel(absoluteRoot, path)
		if relErr != nil {
			relative = path
		}
		for lineNumber, line := range strings.Split(string(data), "\n") {
			if len(matches) >= maxAgentSearchMatches {
				truncated = true
				return false
			}
			if regex.MatchString(line) {
				matches = append(matches, fmt.Sprintf("%s:%d: %s", relative, lineNumber+1, strings.TrimSpace(line)))
			}
		}
		return true
	}

	if info.IsDir() {
		walkErr := filepath.WalkDir(absoluteRoot, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() {
				return nil
			}
			if !scanFile(path) {
				return fs.SkipAll
			}
			return nil
		})
		if walkErr != nil {
			return walkErr.Error(), false
		}
	} else {
		scanFile(absoluteRoot)
	}

	if len(matches) == 0 {
		return fmt.Sprintf("no matches for %q under %s", pattern, absoluteRoot), true
	}
	header := fmt.Sprintf("%d match(es) for %q under %s", len(matches), pattern, absoluteRoot)
	if truncated {
		header += fmt.Sprintf(" (truncated at %d)", maxAgentSearchMatches)
	}
	return header + "\n" + strings.Join(matches, "\n"), true
}

func recordAgentFailure(sess *agentSession, message string) bool {
	sess.mu.Lock()
	if sess.AgentRun == nil {
		sess.mu.Unlock()
		return false
	}
	sess.AgentRun.ConsecutiveErrors++
	failures := sess.AgentRun.ConsecutiveErrors
	sess.mu.Unlock()
	sess.appendAgentEvent(AgentEvent{Kind: "error", Text: message})
	if failures >= 3 {
		finishAgent(sess, agentFailed, "", message)
		return false
	}
	return true
}

func recordAgentRevision(sess *agentSession, message string, evidenceEventIDs []int) bool {
	sess.mu.Lock()
	if sess.AgentRun == nil {
		sess.mu.Unlock()
		return false
	}
	sess.AgentRun.ConsecutiveErrors = 0
	sess.mu.Unlock()
	sess.appendAgentEvent(AgentEvent{Kind: "verification", Action: "answer", Text: message, EvidenceEventIDs: evidenceEventIDs})
	return true
}

func resetAgentFailures(sess *agentSession) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.AgentRun != nil {
		sess.AgentRun.ConsecutiveErrors = 0
	}
}

func finishAgent(sess *agentSession, status AgentRunStatus, answer, errorText string) {
	sess.finishAgentRun(status, answer, errorText)
	text := errorText
	if answer != "" {
		text = answer
	}
	sess.appendAgentEvent(AgentEvent{Kind: "status", Text: string(status) + ": " + text})
	logAgentPersistenceStats(sess)
	cleanupAgentRunResources(sess)
}

// logAgentPersistenceStats reports accumulated persist() overhead (call
// count, bytes written, total time) for a run that just reached a terminal
// state, so real usage can show whether the frequent persist-before/
// persist-after writes are material before considering batching or removing
// any of them — see AGENT.md #9, "measure first".
func logAgentPersistenceStats(sess *agentSession) {
	run := sess.agentSnapshot()
	if run == nil {
		return
	}
	count, totalBytes, totalDuration := sess.persistenceStats()
	log.Printf("[agent-persist][%s] calls=%d totalBytes=%d totalDuration=%s", run.ID, count, totalBytes, totalDuration.Round(time.Millisecond))
}

func cleanupAgentRunResources(sess *agentSession) {
	run := sess.agentSnapshot()
	if run != nil {
		jobStore.cancelRun(run.ID)
	}
	sess.mu.Lock()
	attachmentPath := sess.AttachedFilePath
	sess.AttachedFile = nil
	sess.AttachedFilePath = ""
	sess.AttachedFiles = nil
	sess.AttachedFilePaths = nil
	sess.AgentStart = 0
	sess.AwaitingDecision = false
	sess.mu.Unlock()
	if attachmentPath != "" {
		if dir, err := reconAgentSessionDir(sess.ID); err == nil {
			_ = os.RemoveAll(dir)
		}
	}
}

func truncateAgentText(text string) string {
	if len(text) <= agentMaxObservation {
		return text
	}
	return text[:agentMaxObservation] + "\n... (truncated)"
}

func (s *agentSession) appendAgentMessage(message ChatMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, message)
	s.Stats.recordMessage(message)
	s.Stats.updateContext(systemPrompt, s.Messages)
}

func logAgentError(sessionID string, err error) {
	fmt.Printf("[AGENT] session=%s persistence error: %v\n", sessionID, err)
}
