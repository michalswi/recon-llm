package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
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

// ollamaHTTPClient is used for all requests to the configured Ollama server. When
// OLLAMA_CA_CERT names a PEM file, its certificate is trusted in addition to the
// system pool, so a self-signed cert on a TLS-terminating reverse proxy works.
var ollamaHTTPClient = newOllamaHTTPClient()

func newOllamaHTTPClient() *http.Client {
	if ollamaInsecureSkipVerify {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		return &http.Client{Transport: transport}
	}
	if strings.TrimSpace(ollamaCACertPath) == "" {
		return http.DefaultClient
	}
	pemData, err := os.ReadFile(ollamaCACertPath)
	if err != nil {
		log.Printf("[ollama] could not read OLLAMA_CA_CERT %q: %v", ollamaCACertPath, err)
		return http.DefaultClient
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemData) {
		log.Printf("[ollama] OLLAMA_CA_CERT %q contained no valid certificates", ollamaCACertPath)
		return http.DefaultClient
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	return &http.Client{Transport: transport}
}

// warnIfInsecureOllamaURL flags plain HTTP to a non-loopback host, since prompts,
// findings, and answers would then cross the network readable and alterable by
// anyone on path (MITM). Use https:// (e.g. via a TLS-terminating reverse proxy
// combined with OLLAMA_CA_CERT for a private CA) or an encrypted tunnel instead.
func warnIfInsecureOllamaURL(rawURL string) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "http" {
		return
	}
	switch parsed.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return
	}
	fmt.Println(warn(fmt.Sprintf("Warning: OLLAMA_URL (%s) uses plain HTTP to a non-loopback host; traffic to Ollama is unencrypted and readable/alterable by a network attacker. Use https:// (optionally with OLLAMA_CA_CERT for a private CA) or an encrypted tunnel.", rawURL)))
}

// warnIfInsecureOllamaTLS flags disabled certificate validation: it lets any
// on-path attacker impersonate the Ollama endpoint undetected, for testing only.
func warnIfInsecureOllamaTLS() {
	if !ollamaInsecureSkipVerify {
		return
	}
	fmt.Println(warn("Warning: OLLAMA_INSECURE_SKIP_VERIFY is enabled; TLS certificate validation for Ollama is disabled, so a network attacker can impersonate the endpoint undetected. OLLAMA_CA_CERT is ignored while this is set. Use only for local testing; prefer fixing the certificate (e.g. add a SAN) or OLLAMA_CA_CERT otherwise."))
}

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
	Options  *ChatOptions  `json:"options,omitempty"`
}

// ChatOptions carries Ollama generation options. NumPredict overrides any
// smaller default baked into a model's Modelfile so long answers (e.g.
// generated code) aren't silently truncated mid-response.
type ChatOptions struct {
	NumPredict int `json:"num_predict,omitempty"`
	NumCtx     int `json:"num_ctx,omitempty"`
}

// ollamaChatOptions builds the options sent with every chat request from the
// configurable ollamaNumPredict/ollamaNumCtx package vars (OLLAMA_NUM_PREDICT,
// OLLAMA_NUM_CTX).
func ollamaChatOptions() *ChatOptions {
	options := &ChatOptions{NumPredict: ollamaNumPredict}
	if ollamaNumCtx > 0 {
		options.NumCtx = ollamaNumCtx
	}
	return options
}

type ChatResponse struct {
	Message ChatMessage `json:"message"`
}

type ToolResponse struct {
	Action           string   `json:"action"`
	Command          string   `json:"command,omitempty"`
	Path             string   `json:"path,omitempty"`
	Pattern          string   `json:"pattern,omitempty"`
	MaxDepth         int      `json:"maxDepth,omitempty"`
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

	// Persistence measurement only (never serialized — persistedAgentState does
	// not reference these): see AGENT.md #9, "measure first" before optimizing
	// the frequent persist() writes.
	persistCount    int
	persistBytes    int64
	persistDuration time.Duration
}

type persistedAgentState struct {
	SessionID string        `json:"sessionId"`
	Messages  []ChatMessage `json:"messages"`
	Stats     Stats         `json:"stats"`
	Run       *AgentRun     `json:"run"`
}

func (s *agentSession) persist() error {
	started := time.Now()
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
	err = os.Rename(tempPath, path)
	s.mu.Lock()
	s.persistCount++
	s.persistBytes += int64(len(data))
	s.persistDuration += time.Since(started)
	s.mu.Unlock()
	return err
}

// persistenceStats reports the accumulated persist() call count, total bytes
// written, and total time spent, for measuring whether the frequent
// persist-before/persist-after writes (AGENT.md #9) are material in practice.
func (s *agentSession) persistenceStats() (count int, totalBytes int64, totalDuration time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistCount, s.persistBytes, s.persistDuration
}

func cloneAgentRun(run *AgentRun) *AgentRun {
	if run == nil {
		return nil
	}
	clone := *run
	clone.Events = append([]AgentEvent(nil), run.Events...)
	clone.CandidateHistory = append([]string(nil), run.CandidateHistory...)
	clone.UserRequirements = append([]string(nil), run.UserRequirements...)
	clone.Plan = clonePlanSteps(run.Plan)
	return &clone
}

// clonePlanSteps deep-copies plan steps, including each step's EvidenceEventIDs
// slice, so callers never share a backing array with the live AgentRun.Plan
// that complete_step mutates in place under sess.mu.
func clonePlanSteps(steps []AgentPlanStep) []AgentPlanStep {
	if steps == nil {
		return nil
	}
	cloned := make([]AgentPlanStep, len(steps))
	for i, step := range steps {
		step.EvidenceEventIDs = append([]int(nil), step.EvidenceEventIDs...)
		cloned[i] = step
	}
	return cloned
}

// BackgroundJob is an immutable snapshot of a background job's state, safe to
// read and share across goroutines without further locking. All mutation goes
// through JobStore methods; the live *exec.Cmd used for cancellation is kept
// internally in jobRecord and never exposed on this type.
type BackgroundJob struct {
	ID        string
	Command   string
	Status    string
	Output    string
	Error     string
	StartTime time.Time
	EndTime   time.Time
	SessionID string
	RunID     string
	Done      chan struct{} `json:"-"`
}

// jobRecord holds the mutable state backing a BackgroundJob snapshot; every
// field access is guarded by JobStore.mu.
type jobRecord struct {
	snapshot BackgroundJob
	cmd      *exec.Cmd
}

type JobStore struct {
	mu   sync.Mutex
	jobs map[string]*jobRecord
}

func newJobStore() *JobStore {
	return &JobStore{jobs: make(map[string]*jobRecord)}
}

func (s *JobStore) get(id string) (BackgroundJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.jobs[id]
	if !ok {
		return BackgroundJob{}, false
	}
	return record.snapshot, true
}

// create registers a new running job, assigning RunID up front so the job is
// always attributable to its owning run, even before its process starts.
func (s *JobStore) create(sessionID, runID, command string) BackgroundJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := BackgroundJob{
		ID:        fmt.Sprintf("job_%d", time.Now().UnixNano()),
		Command:   command,
		Status:    "running",
		StartTime: time.Now(),
		SessionID: sessionID,
		RunID:     runID,
		Done:      make(chan struct{}),
	}
	s.jobs[job.ID] = &jobRecord{snapshot: job}
	return job
}

// attachCmd records the process handle used later for cancellation.
func (s *JobStore) attachCmd(id string, cmd *exec.Cmd) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record, ok := s.jobs[id]; ok {
		record.cmd = cmd
	}
}

func (s *JobStore) listBySession(sessionID string) []BackgroundJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	var jobs []BackgroundJob
	for _, record := range s.jobs {
		if record.snapshot.SessionID == sessionID {
			jobs = append(jobs, record.snapshot)
		}
	}
	return jobs
}

// finish records a job's natural completion (success, failure, or timeout).
// It only applies if the job is still "running" so a concurrent explicit
// cancel (see cancel) is never overwritten by a late failed/completed result.
func (s *JobStore) finish(id, status, output, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.jobs[id]
	if !ok {
		return
	}
	record.snapshot.Output = output
	if record.snapshot.Status != "running" {
		return
	}
	record.snapshot.Status = status
	record.snapshot.Error = errText
	record.snapshot.EndTime = time.Now()
}

// cancel kills the job's owned process group, if any, and marks the job
// cancelled unless it already reached a terminal state, in which case that
// state (completed/failed/timeout) is preserved. Returns false if no job with
// this ID and session was found.
func (s *JobStore) cancel(id, sessionID string) bool {
	s.mu.Lock()
	record, ok := s.jobs[id]
	if !ok || record.snapshot.SessionID != sessionID {
		s.mu.Unlock()
		return false
	}
	cmd := record.cmd
	s.mu.Unlock()

	if cmd != nil {
		killProcessGroup(cmd)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok = s.jobs[id]
	if !ok {
		return false
	}
	if record.snapshot.Status == "running" {
		record.snapshot.Status = "cancelled"
		record.snapshot.EndTime = time.Now()
	}
	return true
}

func (s *JobStore) cancelRun(runID string) {
	s.mu.Lock()
	var cmds []*exec.Cmd
	for _, record := range s.jobs {
		if record.snapshot.RunID == runID && record.snapshot.Status == "running" {
			cmds = append(cmds, record.cmd)
		}
	}
	s.mu.Unlock()
	for _, cmd := range cmds {
		if cmd != nil {
			killProcessGroup(cmd)
		}
	}
}

var jobStore = newJobStore()

// executeBackgroundCommand starts the process synchronously (so its *exec.Cmd.Process
// is fully initialized before this function returns and before the job is visible to
// cancel_job/cancelRun) and waits for completion or timeout in a background goroutine.
func executeBackgroundCommand(sessionID, runID, command string) (BackgroundJob, error) {
	command = sanitizeCommand(command)
	if command == "" {
		return BackgroundJob{}, errors.New("empty command after sanitization")
	}
	job := jobStore.create(sessionID, runID, command)
	cmd := exec.Command("bash", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		jobStore.finish(job.ID, "failed", "", err.Error())
		close(job.Done)
		return job, nil
	}
	jobStore.attachCmd(job.ID, cmd)

	go func() {
		defer close(job.Done)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			errText := ""
			status := "completed"
			if err != nil {
				status = "failed"
				errText = err.Error()
				if stderr.Len() > 0 {
					errText += "\n" + stderr.String()
				}
			}
			jobStore.finish(job.ID, status, stdout.String(), errText)
		case <-time.After(time.Hour):
			killProcessGroup(cmd)
			jobStore.finish(job.ID, "timeout", stdout.String(), "job exceeded maximum duration of 1h")
		}
	}()
	return job, nil
}

func callOllamaMessageContext(ctx context.Context, messages []ChatMessage, forceJSON, think bool) (ChatMessage, error) {
	requestBody := ChatRequest{Model: modelName, Messages: messages, Stream: false, Think: think, Options: ollamaChatOptions()}
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
	response, err := ollamaHTTPClient.Do(request)
	if err != nil {
		return ChatMessage{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ChatMessage{}, &ollamaHTTPStatusError{StatusCode: response.StatusCode, Status: response.Status}
	}
	var chatResponse ChatResponse
	if err := json.NewDecoder(response.Body).Decode(&chatResponse); err != nil {
		return ChatMessage{}, err
	}
	return chatResponse.Message, nil
}

// ollamaHTTPStatusError carries the HTTP status code from a non-2xx Ollama
// response so callers can distinguish a retryable rate-limit/server error
// (429/5xx) from a deterministic client error (any other 4xx).
type ollamaHTTPStatusError struct {
	StatusCode int
	Status     string
}

func (e *ollamaHTTPStatusError) Error() string {
	return fmt.Sprintf("ollama returned %s", e.Status)
}

// isRetryableOllamaTransportError reports whether err is a transient transport
// failure (network error, request timeout) or an HTTP 429/5xx response — the
// only classes of Ollama call failure worth retrying. Deterministic 4xx
// responses and undecodable response bodies are not retried; those represent a
// genuinely invalid request or response, not a transient blip.
func isRetryableOllamaTransportError(err error) bool {
	var statusErr *ollamaHTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == http.StatusTooManyRequests || statusErr.StatusCode >= 500
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// ollamaRetryBaseBackoff is the initial backoff before the first retry,
// doubling each subsequent attempt; overridable in tests to avoid real delays.
var ollamaRetryBaseBackoff = 500 * time.Millisecond

// callOllamaMessageWithRetry retries callOllamaMessageWithLogContext for
// transient transport failures and HTTP 429/5xx responses only, with a small
// capped backoff, so a flaky endpoint doesn't consume the agent's 3-strike
// failure budget before its reasoning ever ran. ctx is the caller's own
// deadline/cancellation (e.g. the run's 30-minute budget): a retry is never
// attempted once ctx is done, and the backoff sleep itself is interruptible by
// ctx, so cancellation always wins over a pending retry.
func callOllamaMessageWithRetry(ctx context.Context, origin string, messages []ChatMessage, forceJSON, think bool) (ChatMessage, error) {
	const maxAttempts = 3
	backoff := ollamaRetryBaseBackoff
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, agentModelTimeout)
		message, err := callOllamaMessageWithLogContext(attemptCtx, origin, messages, forceJSON, think)
		cancel()
		if err == nil {
			return message, nil
		}
		lastErr = err
		if ctx.Err() != nil || attempt == maxAttempts || !isRetryableOllamaTransportError(err) {
			return ChatMessage{}, err
		}
		select {
		case <-ctx.Done():
			return ChatMessage{}, lastErr
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return ChatMessage{}, lastErr
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

// ollamaSiblingURL builds a sibling endpoint (e.g. /api/tags) under the same base
// path as the configured chat URL, so a reverse-proxy/gateway path prefix (e.g.
// /gateway/recon) is preserved instead of being dropped in favor of the host root.
func ollamaSiblingURL(parsed *url.URL, endpoint string) string {
	base := strings.TrimSuffix(parsed.Path, "/api/chat")
	return fmt.Sprintf("%s://%s%s%s", parsed.Scheme, parsed.Host, base, endpoint)
}

// checkOllamaReachable queries the Ollama server's model list to confirm it is up,
// without invoking the chat/completion endpoint.
func checkOllamaReachable(ctx context.Context, chatURL string) (string, error) {
	parsed, err := url.Parse(chatURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid OLLAMA_URL %q", chatURL)
	}
	tagsURL := ollamaSiblingURL(parsed, "/api/tags")

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, tagsURL, nil)
	if err != nil {
		return "", err
	}
	response, err := ollamaHTTPClient.Do(request)
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
	tagsURL := ollamaSiblingURL(parsed, "/api/tags")

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, tagsURL, nil)
	if err != nil {
		return "", err
	}
	response, err := ollamaHTTPClient.Do(request)
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
	psURL := ollamaSiblingURL(parsed, "/api/ps")

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, psURL, nil)
	if err != nil {
		return "", err
	}
	response, err := ollamaHTTPClient.Do(request)
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
