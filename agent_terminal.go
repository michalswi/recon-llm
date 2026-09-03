package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

type FileAttachment struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
}

type AgentRequest struct {
	Goal               string   `json:"goal"`
	ExpectedOutput     string   `json:"expectedOutput,omitempty"`
	Constraints        string   `json:"constraints,omitempty"`
	CompletionCriteria string   `json:"completionCriteria,omitempty"`
	FilePath           string   `json:"filePath,omitempty"`
	FilePaths          []string `json:"filePaths,omitempty"`
}

const (
	maxAgentAttachments = 16
	maxAgentImageSize   = 20 << 20
)

var terminalAttachmentPaths []string

func main() {
	checkOllamaFlag := flag.Bool("check-ollama", false, "check whether the configured Ollama server is reachable, then exit")
	checkOllamaPSFlag := flag.Bool("check-ollama-ps", false, "show models currently loaded in Ollama (/api/ps), then exit")
	checkOllamaModelsFlag := flag.Bool("check-ollama-models", false, "show models available in Ollama (/api/tags), then exit")
	apiAddressFlag := flag.String("api-address", agentAPIAddress, "run the agent HTTP API at this address")
	flag.Parse()

	if *checkOllamaFlag {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		message, err := checkOllamaReachable(ctx, ollamaURL)
		if err != nil {
			fmt.Println(warn("Ollama unreachable:"), err)
			os.Exit(1)
		}
		fmt.Println(success(message))
		return
	}

	if *checkOllamaPSFlag {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		message, err := checkOllamaPS(ctx, ollamaURL)
		if err != nil {
			fmt.Println(warn("Ollama unreachable:"), err)
			os.Exit(1)
		}
		fmt.Println(success(message))
		return
	}

	if *checkOllamaModelsFlag {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		message, err := checkOllamaModels(ctx, ollamaURL)
		if err != nil {
			fmt.Println(warn("Ollama unreachable:"), err)
			os.Exit(1)
		}
		fmt.Println(success(message))
		return
	}

	if strings.TrimSpace(*apiAddressFlag) != "" {
		if err := runAgentAPIServer(strings.TrimSpace(*apiAddressFlag)); err != nil {
			fmt.Println(warn("Agent API failed:"), err)
		}
		return
	}

	ShowBanner()
	startTime = time.Now()
	reader := bufio.NewReader(os.Stdin)
	fmt.Println(info("Enter an objective to start an agent run."))
	fmt.Println(info("Use /agent for the structured goal form or /h for help.\n"))

	for {
		fmt.Print(goalLabel("[goal]: "))
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			fmt.Println(warn("Read error:"), err)
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			if errors.Is(err, io.EOF) {
				return
			}
			continue
		}
		if strings.EqualFold(line, "exit") || strings.EqualFold(line, "quit") || line == "/q" {
			return
		}

		var request AgentRequest
		if strings.HasPrefix(strings.ToLower(line), "/agent") {
			initialGoal := strings.TrimSpace(strings.TrimPrefix(line, strings.Fields(line)[0]))
			request, err = readStructuredAgentRequest(reader, initialGoal)
		} else if strings.HasPrefix(line, "/") {
			if handleAgentCLICommand(line) {
				continue
			}
			fmt.Println(warn("Unknown command:"), line)
			continue
		} else {
			request = AgentRequest{Goal: line}
		}
		if err != nil {
			fmt.Println(warn("Input error:"), err)
			continue
		}
		request.FilePaths = append(request.FilePaths, terminalAttachmentPaths...)
		if err := runTerminalAgent(reader, request); err != nil {
			fmt.Println(warn("Agent run failed:"), err)
		}
	}
}

func handleAgentCLICommand(input string) bool {
	parts := strings.Fields(input)
	switch strings.ToLower(parts[0]) {
	case "/h", "/help":
		printHelp()
	case "/dir":
		workingDirectory, err := os.Getwd()
		if err != nil {
			fmt.Println(warn("Working directory unavailable:"), err)
		} else {
			fmt.Println(info(workingDirectory))
		}
	case "/m":
		fmt.Println(info("Model: " + modelName))
	case "/up":
		fmt.Println(info("Uptime: " + formatUptime()))
	case "/ollama":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		message, err := checkOllamaReachable(ctx, ollamaURL)
		cancel()
		if err != nil {
			fmt.Println(warn("Ollama unreachable:"), err)
		} else {
			fmt.Println(success(message))
		}
	case "/ollama-ps":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		message, err := checkOllamaPS(ctx, ollamaURL)
		cancel()
		if err != nil {
			fmt.Println(warn("Ollama unreachable:"), err)
		} else {
			fmt.Println(success(message))
		}
	case "/ollama-models":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		message, err := checkOllamaModels(ctx, ollamaURL)
		cancel()
		if err != nil {
			fmt.Println(warn("Ollama unreachable:"), err)
		} else {
			fmt.Println(success(message))
		}
	case "/attach":
		path := strings.TrimSpace(input[len(parts[0]):])
		paths, err := resolveAttachmentPaths(path)
		if err != nil {
			fmt.Println(warn("Attachment error:"), err)
			break
		}
		updated := appendUniquePaths(append([]string(nil), terminalAttachmentPaths...), paths...)
		if len(updated) > maxAgentAttachments {
			fmt.Printf("%s maximum is %d files.\n", warn("Attachment error:"), maxAgentAttachments)
			break
		}
		terminalAttachmentPaths = updated
		fmt.Printf("%s %d file(s) attached for subsequent runs.\n", success("Attached:"), len(terminalAttachmentPaths))
	case "/attached":
		if len(terminalAttachmentPaths) == 0 {
			fmt.Println(info("No persistent attachments."))
			break
		}
		fmt.Println(accent("Attached files:"))
		for _, path := range terminalAttachmentPaths {
			fmt.Println("  " + path)
		}
	case "/deattach", "/detach":
		terminalAttachmentPaths = nil
		fmt.Println(info("Persistent attachments cleared."))
	case "/sysprompt":
		printSystemPrompt()
	case "/editsysprompt":
		handleEditSystemPrompt()
	case "/think-on":
		thinkingEnabled = true
		fmt.Println(success("Thinking enabled."))
	case "/think-off":
		thinkingEnabled = false
		fmt.Println(info("Thinking disabled."))
	default:
		return false
	}
	return true
}

func readStructuredAgentRequest(reader *bufio.Reader, initialGoal string) (AgentRequest, error) {
	request := AgentRequest{Goal: strings.TrimSpace(initialGoal)}
	var err error
	if request.Goal == "" {
		request.Goal, err = promptLine(reader, "Objective: ")
		if err != nil {
			return AgentRequest{}, err
		}
	}
	request.ExpectedOutput, err = promptLine(reader, "Expected output (optional): ")
	if err != nil {
		return AgentRequest{}, err
	}
	request.Constraints, err = promptLine(reader, "Constraints (optional): ")
	if err != nil {
		return AgentRequest{}, err
	}
	request.CompletionCriteria, err = promptLine(reader, "Completion criteria (optional): ")
	if err != nil {
		return AgentRequest{}, err
	}
	request.FilePath, err = promptLine(reader, "Attachment path (optional): ")
	return request, err
}

func runTerminalAgent(reader *bufio.Reader, request AgentRequest) error {
	sess, err := newTerminalAgentSession(request)
	if err != nil {
		return err
	}

	signalContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSignals()

	for {
		run := sess.agentSnapshot()
		if run == nil {
			return errors.New("agent run was not initialized")
		}

		if run.Status == agentRunning {
			runContext, cancel := agentRunContext(signalContext)
			sess.mu.Lock()
			sess.agentCancel = cancel
			sess.mu.Unlock()
			runAgent(runContext, sess)
			cancel()
			if signalContext.Err() != nil {
				if current := sess.agentSnapshot(); current != nil && current.Status == agentRunning {
					finishAgent(sess, agentCancelled, "", "stopped by user")
				}
				fmt.Println(warn("Agent stopped."))
				return nil
			}
			run = sess.agentSnapshot()
		}

		switch run.Status {
		case agentWaitingInput:
			feedback, err := promptLine(reader, "Clarification: ")
			if err != nil {
				return err
			}
			if feedback == "" {
				fmt.Println(warn("Clarification cannot be empty."))
				continue
			}
			resumeAgent(sess, feedback)
		case agentWaitingApproval:
			fmt.Println(separatorLine())
			fmt.Println(assistantLabel(), run.FinalAnswer)
			decision, err := promptLine(reader, "Accept [a], revise [r], or stop [s]: ")
			if err != nil {
				return err
			}
			switch strings.ToLower(decision) {
			case "a", "accept":
				finishAgent(sess, agentCompleted, run.FinalAnswer, "")
				_ = sess.persist()
				fmt.Println(success("Agent result accepted."))
				return nil
			case "r", "revise":
				feedback, err := promptLine(reader, "Revision feedback: ")
				if err != nil {
					return err
				}
				if feedback == "" {
					fmt.Println(warn("Revision feedback cannot be empty."))
					continue
				}
				resumeAgent(sess, feedback)
			case "s", "stop":
				finishAgent(sess, agentCancelled, "", "stopped by user")
				_ = sess.persist()
				return nil
			default:
				fmt.Println(warn("Choose a, r, or s."))
			}
		case agentCompleted:
			fmt.Println(success("Agent run completed."))
			return nil
		case agentFailed, agentCancelled, agentLimitReached:
			if run.Error != "" {
				return fmt.Errorf("agent %s: %s", run.Status, run.Error)
			}
			return fmt.Errorf("agent %s", run.Status)
		default:
			return fmt.Errorf("unsupported agent status %q", run.Status)
		}
	}
}

func newTerminalAgentSession(request AgentRequest) (*agentSession, error) {
	request.Goal = strings.TrimSpace(request.Goal)
	if request.Goal == "" {
		return nil, errors.New("objective is required")
	}
	attachmentPaths := append([]string(nil), request.FilePaths...)
	if strings.TrimSpace(request.FilePath) != "" {
		attachmentPaths = append(attachmentPaths, request.FilePath)
	}
	if len(attachmentPaths) == 0 {
		if path := imagePathFromGoal(request.Goal); path != "" {
			attachmentPaths = append(attachmentPaths, path)
		}
	}
	attachmentPaths = appendUniquePaths(nil, attachmentPaths...)
	if len(attachmentPaths) > maxAgentAttachments {
		return nil, fmt.Errorf("more than %d attachment paths provided", maxAgentAttachments)
	}

	sessionID := fmt.Sprintf("run_%d", time.Now().UnixNano())
	sess := &agentSession{
		ID:                 sessionID,
		CreatedAt:          time.Now().UTC(),
		AgentMode:          true,
		AgentGoal:          request.Goal,
		OriginalPrompt:     systemPrompt,
		OriginalPromptName: systemPromptName,
		AgentStart:         0,
		ThinkingEnabled:    thinkingEnabled,
	}
	sess.eventSink = printAgentEvent

	goalDescription := request.Goal
	resolvedAttachments := make(map[string]bool)
	for _, attachmentPath := range attachmentPaths {
		paths, err := resolveAttachmentPaths(attachmentPath)
		if err != nil {
			return nil, err
		}
		for _, absolutePath := range paths {
			if resolvedAttachments[absolutePath] {
				continue
			}
			if len(resolvedAttachments) >= maxAgentAttachments {
				return nil, fmt.Errorf("more than %d files resolved from attachment paths", maxAgentAttachments)
			}
			resolvedAttachments[absolutePath] = true
			fileInfo, err := os.Stat(absolutePath)
			if err != nil {
				return nil, fmt.Errorf("open attachment: %w", err)
			}
			attachment := &FileAttachment{Name: fileInfo.Name(), Size: fileInfo.Size()}
			if isSupportedImagePath(absolutePath) {
				if fileInfo.Size() > maxAgentImageSize {
					return nil, fmt.Errorf("image attachment exceeds %d MiB limit", maxAgentImageSize>>20)
				}
				data, err := os.ReadFile(absolutePath)
				if err != nil {
					return nil, fmt.Errorf("read image attachment: %w", err)
				}
				attachment.Type = "image"
				attachment.Content = base64.StdEncoding.EncodeToString(data)
			}
			if sess.AttachedFile == nil {
				sess.AttachedFile = attachment
				sess.AttachedFilePath = absolutePath
			}
			sess.AttachedFiles = append(sess.AttachedFiles, attachment)
			sess.AttachedFilePaths = append(sess.AttachedFilePaths, absolutePath)
			goalDescription += fmt.Sprintf("\n\nFILE AVAILABLE: %s (%d bytes) at path: %s", fileInfo.Name(), fileInfo.Size(), absolutePath)
			if attachment.Type == "image" {
				goalDescription += "\nThe image is attached directly to the model message for visual analysis."
			} else {
				goalDescription += "\nUse the allowed command-line tools to inspect this file when needed."
			}
		}
	}

	if _, err := composeAgentPrompt(systemPrompt, goalDescription, "No previous attempts yet."); err != nil {
		return nil, fmt.Errorf("load agent prompt: %w", err)
	}
	run := newAgentRun(sessionID, goalDescription, systemPrompt, systemPromptName)
	run.GoalSpec = normalizeAgentGoalSpec(AgentGoalSpec{
		Objective:          request.Goal,
		ExpectedOutput:     request.ExpectedOutput,
		Constraints:        request.Constraints,
		CompletionCriteria: request.CompletionCriteria,
	})
	sess.AgentRun = run
	if err := sess.persist(); err != nil {
		return nil, err
	}
	return sess, nil
}

func imagePathFromGoal(goal string) string {
	for _, field := range strings.Fields(goal) {
		candidate := strings.Trim(field, "\"'(),.;:[]{}")
		if filepath.IsAbs(candidate) && isSupportedImagePath(candidate) {
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				return candidate
			}
		}
	}
	return ""
}

func isSupportedImagePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		return true
	default:
		return false
	}
}

func resolveAttachmentPaths(path string) ([]string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve attachment path: %w", err)
	}
	info, err := os.Stat(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("open attachment: %w", err)
	}
	if info.Mode().IsRegular() {
		return []string{absolutePath}, nil
	}
	if !info.IsDir() {
		return nil, errors.New("attachment must be a regular file or directory")
	}
	entries, err := os.ReadDir(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("read attachment directory: %w", err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			paths = append(paths, filepath.Join(absolutePath, entry.Name()))
		}
	}
	if len(paths) == 0 {
		return nil, errors.New("attachment directory contains no regular files")
	}
	if len(paths) > maxAgentAttachments {
		return nil, fmt.Errorf("attachment directory contains more than %d regular files", maxAgentAttachments)
	}
	return paths, nil
}

func appendUniquePaths(existing []string, paths ...string) []string {
	seen := make(map[string]bool, len(existing)+len(paths))
	for _, path := range existing {
		seen[path] = true
	}
	for _, path := range paths {
		if path != "" && !seen[path] {
			existing = append(existing, path)
			seen[path] = true
		}
	}
	return existing
}

func resumeAgent(sess *agentSession, feedback string) {
	feedback = strings.TrimSpace(feedback)
	sess.appendAgentEvent(AgentEvent{Kind: "feedback", Text: feedback})
	sess.mu.Lock()
	sess.AgentRun.UserRequirements = append(sess.AgentRun.UserRequirements, feedback)
	sess.AwaitingDecision = false
	sess.AgentMode = true
	sess.AgentRun.Status = agentRunning
	sess.AgentRun.FinalAnswer = ""
	sess.AgentRun.UpdatedAt = time.Now().UTC()
	sess.mu.Unlock()
}

func printAgentEvent(event AgentEvent) {
	switch event.Kind {
	case "action":
		if event.Command != "" {
			fmt.Printf("%s %s\n", runningLabel(), event.Command)
		}
	case "observation":
		if event.Output != "" {
			fmt.Println(outputLabel())
			fmt.Println(event.Output)
		} else if event.Text != "" {
			fmt.Println(info(event.Text))
		}
	case "plan":
		fmt.Println(accent("Plan:"))
		fmt.Println(event.Text)
	case "clarification":
		fmt.Println(accent("Agent needs clarification:"), event.Text)
	case "verification":
		fmt.Println(info("Critic requested revision:"), event.Text)
	case "error", "invalid_action":
		fmt.Println(warn("Agent error:"), formatAgentDisplayText(event.Text))
	}
}

const maxAgentDisplayTextLength = 2000

// formatAgentDisplayText renders raw, possibly-undecoded model text for the terminal only;
// persisted events keep the original, unmodified text.
func formatAgentDisplayText(text string) string {
	text = strings.ReplaceAll(text, "\\r\\n", "\n")
	text = strings.ReplaceAll(text, "\\n", "\n")
	text = strings.ReplaceAll(text, "\\t", "\t")
	text = strings.ReplaceAll(text, "\r", "")
	if len(text) > maxAgentDisplayTextLength {
		text = text[:maxAgentDisplayTextLength] + "\n... (truncated for display)"
	}
	return strings.TrimSpace(text)
}

func promptLine(reader *bufio.Reader, label string) (string, error) {
	fmt.Print(accent(label))
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func composeAgentPrompt(basePrompt, goal, history string) (string, error) {
	data, err := embeddedPrompts.ReadFile("prompts/agent.txt")
	if err != nil {
		return "", err
	}
	protocol := strings.ReplaceAll(string(data), "{GOAL_DESCRIPTION}", goal)
	protocol = strings.ReplaceAll(protocol, "{ITERATION_HISTORY}", history)
	return strings.TrimSpace(basePrompt) + "\n\n---\n\n" + protocol, nil
}
