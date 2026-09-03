package main

import (
	"bufio"
	"context"
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
	Goal               string `json:"goal"`
	ExpectedOutput     string `json:"expectedOutput,omitempty"`
	Constraints        string `json:"constraints,omitempty"`
	CompletionCriteria string `json:"completionCriteria,omitempty"`
	FilePath           string `json:"filePath,omitempty"`
}

func main() {
	apiAddressFlag := flag.String("api-address", agentAPIAddress, "run the agent HTTP API at this address")
	flag.Parse()
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
		fmt.Print(accent("Goal: "))
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
	if strings.TrimSpace(request.FilePath) != "" {
		absolutePath, err := filepath.Abs(strings.TrimSpace(request.FilePath))
		if err != nil {
			return nil, fmt.Errorf("resolve attachment path: %w", err)
		}
		fileInfo, err := os.Stat(absolutePath)
		if err != nil {
			return nil, fmt.Errorf("open attachment: %w", err)
		}
		if !fileInfo.Mode().IsRegular() {
			return nil, errors.New("attachment must be a regular file")
		}
		sess.AttachedFile = &FileAttachment{Name: fileInfo.Name(), Size: fileInfo.Size()}
		sess.AttachedFilePath = absolutePath
		goalDescription += fmt.Sprintf("\n\nFILE AVAILABLE: %s (%d bytes) at path: %s", fileInfo.Name(), fileInfo.Size(), absolutePath)
		goalDescription += "\nUse the allowed command-line tools to inspect this file when needed."
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
		fmt.Println(warn("Agent error:"), event.Text)
	}
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
	data, err := os.ReadFile("prompts/agent.txt")
	if err != nil {
		return "", err
	}
	protocol := strings.ReplaceAll(string(data), "{GOAL_DESCRIPTION}", goal)
	protocol = strings.ReplaceAll(protocol, "{ITERATION_HISTORY}", history)
	return strings.TrimSpace(basePrompt) + "\n\n---\n\n" + protocol, nil
}
