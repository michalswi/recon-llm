package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxAgentAPIRequestSize = 1 << 20

type agentAPI struct {
	mu   sync.RWMutex
	runs map[string]*apiAgentRun
}

type apiAgentRun struct {
	mu      sync.Mutex
	session *agentSession
	running bool
}

type agentAPIRunResponse struct {
	ID                  string          `json:"id"`
	GoalSpec            AgentGoalSpec   `json:"goalSpec"`
	Status              AgentRunStatus  `json:"status"`
	Iteration           int             `json:"iteration"`
	MaxIterations       int             `json:"maxIterations"`
	StartedAt           time.Time       `json:"startedAt"`
	UpdatedAt           time.Time       `json:"updatedAt"`
	CompletedAt         time.Time       `json:"completedAt,omitempty"`
	FinalAnswer         string          `json:"finalAnswer,omitempty"`
	Error               string          `json:"error,omitempty"`
	Events              []AgentEvent    `json:"events"`
	Plan                []AgentPlanStep `json:"plan,omitempty"`
	AvailableCommands   []string        `json:"availableCommands"`
	UnavailableCommands []string        `json:"unavailableCommands,omitempty"`
}

type agentAPIFeedbackRequest struct {
	Feedback string `json:"feedback"`
}

func newAgentAPI() *agentAPI {
	return &agentAPI{runs: make(map[string]*apiAgentRun)}
}

func (api *agentAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", api.handleHealth)
	mux.HandleFunc("/api/agent/runs", api.handleRuns)
	mux.HandleFunc("/api/agent/runs/", api.handleRun)
	return mux
}

func runAgentAPIServer(address string) error {
	server := &http.Server{
		Addr:              address,
		Handler:           newAgentAPI().handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
	}
	fmt.Printf("Agent API listening on http://%s\n", address)
	return server.ListenAndServe()
}

func (api *agentAPI) handleHealth(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeAgentAPIError(response, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeAgentAPIJSON(response, http.StatusOK, map[string]string{"status": "ok"})
}

func (api *agentAPI) handleRuns(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/api/agent/runs" {
		writeAgentAPIError(response, http.StatusNotFound, "not found")
		return
	}
	if request.Method != http.MethodPost {
		writeAgentAPIError(response, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var agentRequest AgentRequest
	if err := decodeAgentAPIJSON(response, request, &agentRequest); err != nil {
		writeAgentAPIError(response, http.StatusBadRequest, err.Error())
		return
	}
	session, err := newTerminalAgentSession(agentRequest)
	if err != nil {
		writeAgentAPIError(response, http.StatusBadRequest, err.Error())
		return
	}
	session.eventSink = nil
	entry := &apiAgentRun{session: session}
	run := session.agentSnapshot()

	api.mu.Lock()
	api.runs[run.ID] = entry
	api.mu.Unlock()
	entry.start()

	writeAgentAPIJSON(response, http.StatusAccepted, newAgentAPIRunResponse(session.agentSnapshot()))
}

func (api *agentAPI) handleRun(response http.ResponseWriter, request *http.Request) {
	path := strings.TrimPrefix(request.URL.Path, "/api/agent/runs/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" || len(parts) > 2 {
		writeAgentAPIError(response, http.StatusNotFound, "not found")
		return
	}

	api.mu.RLock()
	entry, ok := api.runs[parts[0]]
	api.mu.RUnlock()
	if !ok {
		writeAgentAPIError(response, http.StatusNotFound, "agent run not found")
		return
	}

	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch {
	case request.Method == http.MethodGet && action == "":
		writeAgentAPIJSON(response, http.StatusOK, newAgentAPIRunResponse(entry.session.agentSnapshot()))
	case request.Method == http.MethodPost && action == "feedback":
		api.handleFeedback(response, request, entry)
	case request.Method == http.MethodPost && action == "approve":
		api.handleApprove(response, entry)
	case request.Method == http.MethodPost && action == "cancel":
		api.handleCancel(response, entry)
	default:
		writeAgentAPIError(response, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (api *agentAPI) handleFeedback(response http.ResponseWriter, request *http.Request, entry *apiAgentRun) {
	var feedbackRequest agentAPIFeedbackRequest
	if err := decodeAgentAPIJSON(response, request, &feedbackRequest); err != nil {
		writeAgentAPIError(response, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(feedbackRequest.Feedback) == "" {
		writeAgentAPIError(response, http.StatusBadRequest, "feedback is required")
		return
	}

	entry.mu.Lock()
	run := entry.session.agentSnapshot()
	if run.Status != agentWaitingInput && run.Status != agentWaitingApproval {
		entry.mu.Unlock()
		writeAgentAPIError(response, http.StatusConflict, "agent is not waiting for feedback")
		return
	}
	resumeAgent(entry.session, feedbackRequest.Feedback)
	entry.mu.Unlock()
	entry.start()
	writeAgentAPIJSON(response, http.StatusAccepted, newAgentAPIRunResponse(entry.session.agentSnapshot()))
}

func (api *agentAPI) handleApprove(response http.ResponseWriter, entry *apiAgentRun) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	run := entry.session.agentSnapshot()
	if run.Status != agentWaitingApproval {
		writeAgentAPIError(response, http.StatusConflict, "agent is not waiting for approval")
		return
	}
	finishAgent(entry.session, agentCompleted, run.FinalAnswer, "")
	if err := entry.session.persist(); err != nil {
		writeAgentAPIError(response, http.StatusInternalServerError, err.Error())
		return
	}
	writeAgentAPIJSON(response, http.StatusOK, newAgentAPIRunResponse(entry.session.agentSnapshot()))
}

func (api *agentAPI) handleCancel(response http.ResponseWriter, entry *apiAgentRun) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	run := entry.session.agentSnapshot()
	if run.Status != agentRunning && run.Status != agentWaitingInput && run.Status != agentWaitingApproval {
		writeAgentAPIError(response, http.StatusConflict, "agent run is already finished")
		return
	}
	entry.session.cancelAgentRun()
	finishAgent(entry.session, agentCancelled, "", "cancelled through API")
	if err := entry.session.persist(); err != nil {
		writeAgentAPIError(response, http.StatusInternalServerError, err.Error())
		return
	}
	writeAgentAPIJSON(response, http.StatusOK, newAgentAPIRunResponse(entry.session.agentSnapshot()))
}

func (entry *apiAgentRun) start() {
	entry.mu.Lock()
	if entry.running {
		entry.mu.Unlock()
		return
	}
	run := entry.session.agentSnapshot()
	if run == nil || run.Status != agentRunning {
		entry.mu.Unlock()
		return
	}
	ctx, cancel := agentRunContext(context.Background())
	entry.session.mu.Lock()
	entry.session.agentCancel = cancel
	entry.session.mu.Unlock()
	entry.running = true
	entry.mu.Unlock()

	go func() {
		runAgent(ctx, entry.session)
		entry.mu.Lock()
		entry.running = false
		current := entry.session.agentSnapshot()
		restart := current != nil && current.Status == agentRunning && ctx.Err() == nil
		if current != nil && current.Status == agentRunning && ctx.Err() != nil {
			status := agentCancelled
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				status = agentLimitReached
			}
			finishAgent(entry.session, status, "", ctx.Err().Error())
		}
		entry.mu.Unlock()
		cancel()
		if restart {
			entry.start()
		}
	}()
}

func newAgentAPIRunResponse(run *AgentRun) agentAPIRunResponse {
	return agentAPIRunResponse{
		ID:                  run.ID,
		GoalSpec:            run.GoalSpec,
		Status:              run.Status,
		Iteration:           run.Iteration,
		MaxIterations:       run.MaxIterations,
		StartedAt:           run.StartedAt,
		UpdatedAt:           run.UpdatedAt,
		CompletedAt:         run.CompletedAt,
		FinalAnswer:         run.FinalAnswer,
		Error:               run.Error,
		Events:              run.Events,
		Plan:                run.Plan,
		AvailableCommands:   run.AvailableCommands,
		UnavailableCommands: run.UnavailableCommands,
	}
}

func decodeAgentAPIJSON(response http.ResponseWriter, request *http.Request, destination any) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(response, request.Body, maxAgentAPIRequestSize))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid JSON request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request must contain exactly one JSON object")
	}
	return nil
}

func writeAgentAPIError(response http.ResponseWriter, status int, message string) {
	writeAgentAPIJSON(response, status, map[string]string{"error": message})
}

func writeAgentAPIJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
