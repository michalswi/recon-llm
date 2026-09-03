package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/michalswi/color"
)

type ChatMessage struct {
	Role     string   `json:"role"`
	Content  string   `json:"content"`
	Thinking string   `json:"thinking,omitempty"`
	Images   []string `json:"images,omitempty"`
}

type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Format   string        `json:"format,omitempty"`
	Think    bool          `json:"think"`
}

type ChatResponse struct {
	Message ChatMessage `json:"message"`
}

type ToolResponse struct {
	Action           string   `json:"action"`
	Command          string   `json:"command,omitempty"`
	Text             string   `json:"text,omitempty"`
	JobID            string   `json:"jobId,omitempty"`
	Steps            []string `json:"steps,omitempty"`
	StepID           int      `json:"stepId,omitempty"`
	EvidenceEventIDs []int    `json:"evidenceEventIds,omitempty"`
}

type Stats struct {
	UserMessages           int
	AssistantMessages      int
	CommandsRun            int
	TotalUserChars         int
	TotalAssistantChars    int
	LastCommand            string
	ContextChars           int
	EstimatedContextTokens int
	LastResponseMillis     int64
}

func (s *Stats) recordMessage(message ChatMessage) {
	switch message.Role {
	case "user":
		s.UserMessages++
		s.TotalUserChars += len(message.Content)
	case "assistant":
		s.AssistantMessages++
		s.TotalAssistantChars += len(message.Content) + len(message.Thinking)
	}
}

func (s *Stats) updateContext(basePrompt string, messages []ChatMessage) {
	contextChars := len(basePrompt)
	for _, message := range messages {
		contextChars += len(message.Content) + len(message.Thinking)
	}
	s.ContextChars = contextChars
	s.EstimatedContextTokens = (contextChars + 3) / 4
}

func (s *Stats) recordCommand(command string) {
	s.CommandsRun++
	s.LastCommand = command
}

type agentSession struct {
	mu                 sync.Mutex
	ID                 string
	Messages           []ChatMessage
	CreatedAt          time.Time
	Stats              Stats
	AgentMode          bool
	AgentGoal          string
	OriginalPrompt     string
	OriginalPromptName string
	CommandCount       int
	PartialFindings    string
	AttachedFile       *FileAttachment
	AttachedFilePath   string
	AttachedFiles      []*FileAttachment
	AttachedFilePaths  []string
	ThinkingEnabled    bool
	AwaitingDecision   bool
	AgentStart         int
	AgentRun           *AgentRun `json:"agentRun,omitempty"`
	agentCancel        context.CancelFunc
	eventSink          func(AgentEvent)
}

type persistedAgentState struct {
	SessionID string        `json:"sessionId"`
	Messages  []ChatMessage `json:"messages"`
	Stats     Stats         `json:"stats"`
	Run       *AgentRun     `json:"run"`
}

func (s *agentSession) persist() error {
	s.mu.Lock()
	state := persistedAgentState{
		SessionID: s.ID,
		Messages:  append([]ChatMessage(nil), s.Messages...),
		Stats:     s.Stats,
		Run:       cloneAgentRun(s.AgentRun),
	}
	s.mu.Unlock()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode agent state: %w", err)
	}
	path, err := reconAgentRunPath(s.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create agent run directory: %w", err)
	}
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write agent state: %w", err)
	}
	return os.Rename(tempPath, path)
}

func cloneAgentRun(run *AgentRun) *AgentRun {
	if run == nil {
		return nil
	}
	clone := *run
	clone.Events = append([]AgentEvent(nil), run.Events...)
	clone.CandidateHistory = append([]string(nil), run.CandidateHistory...)
	clone.UserRequirements = append([]string(nil), run.UserRequirements...)
	clone.Plan = append([]AgentPlanStep(nil), run.Plan...)
	return &clone
}

type BackgroundJob struct {
	ID        string
	Command   string
	Status    string
	Output    string
	Error     string
	StartTime time.Time
	EndTime   time.Time
	Cmd       *exec.Cmd
	SessionID string
	RunID     string
	Done      chan struct{} `json:"-"`
}

type JobStore struct {
	mu   sync.RWMutex
	jobs map[string]*BackgroundJob
}

func newJobStore() *JobStore {
	return &JobStore{jobs: make(map[string]*BackgroundJob)}
}

func (s *JobStore) get(id string) (*BackgroundJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	return job, ok
}

func (s *JobStore) create(sessionID, command string) *BackgroundJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := &BackgroundJob{
		ID:        fmt.Sprintf("job_%d", time.Now().UnixNano()),
		Command:   command,
		Status:    "running",
		StartTime: time.Now(),
		SessionID: sessionID,
		Done:      make(chan struct{}),
	}
	s.jobs[job.ID] = job
	return job
}

func (s *JobStore) listBySession(sessionID string) []*BackgroundJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var jobs []*BackgroundJob
	for _, job := range s.jobs {
		if job.SessionID == sessionID {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

func (s *JobStore) update(job *BackgroundJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.ID] = job
}

func (s *JobStore) cancelRun(runID string) {
	s.mu.RLock()
	var jobs []*BackgroundJob
	for _, job := range s.jobs {
		if job.RunID == runID && job.Status == "running" {
			jobs = append(jobs, job)
		}
	}
	s.mu.RUnlock()
	for _, job := range jobs {
		if job.Cmd != nil && job.Cmd.Process != nil {
			_ = job.Cmd.Process.Kill()
		}
	}
}

var jobStore = newJobStore()

func executeBackgroundCommand(sessionID, command string) (*BackgroundJob, error) {
	command = sanitizeCommand(command)
	if command == "" {
		return nil, errors.New("empty command after sanitization")
	}
	job := jobStore.create(sessionID, command)
	cmd := exec.Command("bash", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	job.Cmd = cmd

	go func() {
		defer close(job.Done)
		done := make(chan error, 1)
		go func() { done <- cmd.Run() }()
		select {
		case err := <-done:
			job.EndTime = time.Now()
			job.Output = stdout.String()
			if err != nil {
				job.Status = "failed"
				job.Error = err.Error()
				if stderr.Len() > 0 {
					job.Error += "\n" + stderr.String()
				}
			} else {
				job.Status = "completed"
			}
		case <-time.After(time.Hour):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			job.EndTime = time.Now()
			job.Status = "timeout"
			job.Error = "job exceeded maximum duration of 1h"
			job.Output = stdout.String()
		}
		jobStore.update(job)
	}()
	return job, nil
}

func callOllamaMessageContext(ctx context.Context, messages []ChatMessage, forceJSON, think bool) (ChatMessage, error) {
	requestBody := ChatRequest{Model: modelName, Messages: messages, Stream: false, Think: think}
	if forceJSON {
		requestBody.Format = "json"
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(requestBody); err != nil {
		return ChatMessage{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaURL, &body)
	if err != nil {
		return ChatMessage{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return ChatMessage{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ChatMessage{}, fmt.Errorf("ollama returned %s", response.Status)
	}
	var chatResponse ChatResponse
	if err := json.NewDecoder(response.Body).Decode(&chatResponse); err != nil {
		return ChatMessage{}, err
	}
	return chatResponse.Message, nil
}

func callOllamaWithLogContext(ctx context.Context, origin string, messages []ChatMessage, forceJSON bool) (string, error) {
	message, err := callOllamaMessageWithLogContext(ctx, origin, messages, forceJSON, thinkingEnabled)
	return message.Content, err
}

func callOllamaMessageWithLogContext(ctx context.Context, origin string, messages []ChatMessage, forceJSON, think bool) (ChatMessage, error) {
	started := time.Now()
	message, err := callOllamaMessageContext(ctx, messages, forceJSON, think)
	log.Printf("[ollama][%s] messages=%d model=%s json-mode=%v think=%v dur=%s err=%v", origin, len(messages), modelName, forceJSON, think, time.Since(started).Round(time.Millisecond), err)
	return message, err
}

// checkOllamaReachable queries the Ollama server's model list to confirm it is up,
// without invoking the chat/completion endpoint.
func checkOllamaReachable(ctx context.Context, chatURL string) (string, error) {
	parsed, err := url.Parse(chatURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid OLLAMA_URL %q", chatURL)
	}
	tagsURL := fmt.Sprintf("%s://%s/api/tags", parsed.Scheme, parsed.Host)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, tagsURL, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("could not reach %s: %w", tagsURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("%s returned %s", tagsURL, response.Status)
	}

	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("unexpected response from %s: %w", tagsURL, err)
	}
	return fmt.Sprintf("Ollama reachable at %s (%d model(s) available)", tagsURL, len(payload.Models)), nil
}

// checkOllamaModels queries Ollama's /api/tags endpoint to list available models.
func checkOllamaModels(ctx context.Context, chatURL string) (string, error) {
	parsed, err := url.Parse(chatURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid OLLAMA_URL %q", chatURL)
	}
	tagsURL := fmt.Sprintf("%s://%s/api/tags", parsed.Scheme, parsed.Host)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, tagsURL, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("could not reach %s: %w", tagsURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("%s returned %s", tagsURL, response.Status)
	}

	var payload struct {
		Models []struct {
			Name       string `json:"name"`
			Size       int64  `json:"size"`
			ModifiedAt string `json:"modified_at"`
		} `json:"models"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("unexpected response from %s: %w", tagsURL, err)
	}
	if len(payload.Models) == 0 {
		return "No models available in Ollama.", nil
	}
	var lines strings.Builder
	fmt.Fprintf(&lines, "%d model(s) available in Ollama:", len(payload.Models))
	for _, model := range payload.Models {
		fmt.Fprintf(&lines, "\n  %s: size=%d modified_at=%s", model.Name, model.Size, model.ModifiedAt)
	}
	return lines.String(), nil
}

// checkOllamaPS queries Ollama's /api/ps endpoint to list currently loaded models.
func checkOllamaPS(ctx context.Context, chatURL string) (string, error) {
	parsed, err := url.Parse(chatURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid OLLAMA_URL %q", chatURL)
	}
	psURL := fmt.Sprintf("%s://%s/api/ps", parsed.Scheme, parsed.Host)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, psURL, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("could not reach %s: %w", psURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("%s returned %s", psURL, response.Status)
	}

	var payload struct {
		Models []struct {
			Name      string `json:"name"`
			Size      int64  `json:"size"`
			SizeVRAM  int64  `json:"size_vram"`
			ExpiresAt string `json:"expires_at"`
		} `json:"models"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("unexpected response from %s: %w", psURL, err)
	}
	if len(payload.Models) == 0 {
		return "No models currently loaded in Ollama.", nil
	}
	var lines strings.Builder
	fmt.Fprintf(&lines, "%d model(s) loaded in Ollama:", len(payload.Models))
	for _, model := range payload.Models {
		fmt.Fprintf(&lines, "\n  %s: size=%d size_vram=%d expires_at=%s", model.Name, model.Size, model.SizeVRAM, model.ExpiresAt)
	}
	return lines.String(), nil
}

func sanitizeCommand(command string) string {
	command = strings.TrimSpace(command)
	command = strings.Trim(command, "`")
	command = strings.TrimSpace(command)
	command = strings.TrimPrefix(command, "$")
	return strings.TrimSpace(command)
}

func allowedCommandsList() []string {
	commands := make([]string, 0, len(allowedCommands))
	for command := range allowedCommands {
		commands = append(commands, command)
	}
	sort.Strings(commands)
	return commands
}

func reconHomeDir() (string, error) {
	homeDirectory, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(homeDirectory, ".recon"), nil
}

func reconAgentSessionDir(sessionID string) (string, error) {
	baseDirectory, err := reconHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(baseDirectory, "agent_files", sessionID), nil
}

func reconAgentRunPath(sessionID string) (string, error) {
	baseDirectory, err := reconHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(baseDirectory, "agent_runs", sessionID+".json"), nil
}

func formatUptime() string {
	if startTime.IsZero() {
		return "0s"
	}
	return time.Since(startTime).Round(time.Second).String()
}

func accent(text string) string    { return color.Format(color.WHITE, text) }
func info(text string) string      { return color.Format(color.GREEN, text) }
func success(text string) string   { return color.Format(color.GREEN, text) }
func warn(text string) string      { return color.Format(color.RED, text) }
func goalLabel(text string) string { return color.Format(color.BLUE, text) }
func assistantLabel() string       { return color.Format(color.GREEN, "Assistant:") }
func runningLabel() string         { return color.Format(color.GREEN, "[Running]:") }
func outputLabel() string          { return color.Format(color.GREEN, "[Command output]:") }
func separatorLine() string        { return color.Format(color.MINGLE, separator) }
