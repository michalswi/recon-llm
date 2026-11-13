package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgentAPILifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var agentCalls atomic.Int32
	ollama := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var chatRequest ChatRequest
		if err := json.NewDecoder(request.Body).Decode(&chatRequest); err != nil {
			t.Errorf("decode Ollama request: %v", err)
			response.WriteHeader(http.StatusBadRequest)
			return
		}

		content := `{"approved":true,"feedback":"","evidenceEventIds":[]}`
		if len(chatRequest.Messages) == 0 || !strings.Contains(chatRequest.Messages[0].Content, "independent critic") {
			if agentCalls.Add(1) == 1 {
				content = `{"action":"request_clarification","text":"Which format?"}`
			} else {
				content = `{"action":"answer","text":"plain text result"}`
			}
		}
		_ = json.NewEncoder(response).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: content}})
	}))
	defer ollama.Close()

	previousOllamaURL := ollamaURL
	ollamaURL = ollama.URL
	t.Cleanup(func() { ollamaURL = previousOllamaURL })

	server := httptest.NewServer(newAgentAPI().handler())
	defer server.Close()

	status, body := agentAPIRequest(t, http.MethodPost, server.URL+"/api/agent/runs", `{"goal":"prepare a result"}`)
	if status != http.StatusAccepted {
		t.Fatalf("create status = %d, body = %s", status, body)
	}
	var created agentAPIRunResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	waitForAgentAPIStatus(t, server.URL, created.ID, agentWaitingInput)
	status, body = agentAPIRequest(t, http.MethodPost, server.URL+"/api/agent/runs/"+created.ID+"/feedback", `{"feedback":"Use plain text"}`)
	if status != http.StatusAccepted {
		t.Fatalf("feedback status = %d, body = %s", status, body)
	}

	waitForAgentAPIStatus(t, server.URL, created.ID, agentWaitingApproval)
	status, body = agentAPIRequest(t, http.MethodPost, server.URL+"/api/agent/runs/"+created.ID+"/approve", "")
	if status != http.StatusOK {
		t.Fatalf("approve status = %d, body = %s", status, body)
	}
	var completed agentAPIRunResponse
	if err := json.Unmarshal(body, &completed); err != nil {
		t.Fatalf("decode approval response: %v", err)
	}
	if completed.Status != agentCompleted || completed.FinalAnswer != "plain text result" {
		t.Fatalf("unexpected completed run: status=%q answer=%q", completed.Status, completed.FinalAnswer)
	}
}

func TestAgentAPIWriteFileLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	path := filepath.Join(root, "main.go")
	content := "package main\n\nfunc main() {}\n"
	var agentCalls atomic.Int32
	var criticSawContent atomic.Bool
	ollama := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var chatRequest ChatRequest
		if err := json.NewDecoder(request.Body).Decode(&chatRequest); err != nil {
			t.Errorf("decode Ollama request: %v", err)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(chatRequest.Messages) > 0 && strings.Contains(chatRequest.Messages[0].Content, "independent critic") {
			if len(chatRequest.Messages) > 1 && strings.Contains(chatRequest.Messages[1].Content, strings.TrimSpace(content)) {
				criticSawContent.Store(true)
			}
			_ = json.NewEncoder(response).Encode(ChatResponse{Message: ChatMessage{Content: `{"approved":true,"feedback":"","evidenceEventIds":[3]}`}})
			return
		}
		var action ToolResponse
		if agentCalls.Add(1) == 1 {
			action = ToolResponse{Action: "write_file", Path: path, Content: &content}
		} else {
			action = ToolResponse{Action: "answer", Text: "Created main.go."}
		}
		encoded, _ := json.Marshal(action)
		_ = json.NewEncoder(response).Encode(ChatResponse{Message: ChatMessage{Content: string(encoded)}})
	}))
	defer ollama.Close()

	previousOllamaURL := ollamaURL
	ollamaURL = ollama.URL
	t.Cleanup(func() { ollamaURL = previousOllamaURL })
	server := httptest.NewServer(newAgentAPI().handler())
	defer server.Close()
	requestBody, _ := json.Marshal(AgentRequest{Goal: "create a simple Go application in main.go", WritableRoots: []string{root}})
	status, body := agentAPIRequest(t, http.MethodPost, server.URL+"/api/agent/runs", string(requestBody))
	if status != http.StatusAccepted {
		t.Fatalf("create status = %d, body = %s", status, body)
	}
	var created agentAPIRunResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if len(created.WritableRoots) != 1 || created.WritableRoots[0] != root {
		t.Fatalf("writable roots = %v, want [%s]", created.WritableRoots, root)
	}
	run := waitForAgentAPIStatus(t, server.URL, created.ID, agentWaitingApproval)
	if run.FinalAnswer != "Created main.go." {
		t.Fatalf("unexpected answer: %q", run.FinalAnswer)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != content {
		t.Fatalf("written file = %q, %v", data, err)
	}
	if len(run.Events) < 3 || run.Events[2].Action != "write_file" || !run.Events[2].Success || run.Events[2].Path != path {
		t.Fatalf("missing successful write evidence: %#v", run.Events)
	}
	encodedEvents, _ := json.Marshal(run.Events)
	if bytes.Contains(encodedEvents, []byte(content)) {
		t.Fatalf("API events leaked file content: %s", encodedEvents)
	}
	snapshotPath, err := reconAgentRunPath(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(snapshot, []byte(content)) {
		t.Fatalf("persisted snapshot leaked file content: %s", snapshot)
	}
	if !criticSawContent.Load() {
		t.Fatal("critic did not receive transient written-file content")
	}
}

func TestAgentAPISkillsLifecycle(t *testing.T) {
	for _, selection := range []string{"explicit", "on-demand"} {
		t.Run(selection, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			skillPath := writeTestSkill(t, "report", "---\nname: report\ndescription: Report observed facts\n---\nSeparate verified facts from assumptions.")
			previousLimit, previousURL := agentIterationLimit, ollamaURL
			agentIterationLimit = 1
			t.Cleanup(func() { agentIterationLimit, ollamaURL = previousLimit, previousURL })
			var calls atomic.Int32
			var criticSawSkill atomic.Bool
			ollama := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				var chat ChatRequest
				if err := json.NewDecoder(request.Body).Decode(&chat); err != nil {
					t.Error(err)
					response.WriteHeader(http.StatusBadRequest)
					return
				}
				content := `{"approved":true,"feedback":"","evidenceEventIds":[]}`
				if strings.Contains(chat.Messages[0].Content, "independent critic") {
					if strings.Contains(chat.Messages[1].Content, "Separate verified facts from assumptions.") {
						criticSawSkill.Store(true)
					}
				} else {
					attempt := calls.Add(1)
					if !strings.Contains(chat.Messages[0].Content, "Report observed facts") {
						t.Error("worker did not receive the skill catalog")
					}
					shouldBeActive := selection == "explicit" || attempt > 1
					if strings.Contains(chat.Messages[0].Content, "Separate verified facts from assumptions.") != shouldBeActive {
						t.Errorf("worker active instructions mismatch on attempt %d", attempt)
					}
					if attempt == 1 {
						content = `{"action":"load_skill","skill":"report"}`
						if selection == "explicit" {
							content = `{"action":"update_findings","text":"Preparing a report"}`
						}
					} else {
						content = `{"action":"answer","text":"Verified facts: no observations supplied. Assumptions: none."}`
					}
				}
				_ = json.NewEncoder(response).Encode(ChatResponse{Message: ChatMessage{Content: content}})
			}))
			defer ollama.Close()
			ollamaURL = ollama.URL
			server := httptest.NewServer(newAgentAPI().handler())
			defer server.Close()

			status, body := agentAPIRequest(t, http.MethodPost, server.URL+"/api/agent/runs", `{"goal":"summarize facts","skills":["missing"]}`)
			if status != http.StatusBadRequest {
				t.Fatalf("unknown explicit skill should return 400, got %d: %s", status, body)
			}
			request := AgentRequest{Goal: "summarize facts"}
			if selection == "explicit" {
				request.Skills = []string{"report"}
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			status, body = agentAPIRequest(t, http.MethodPost, server.URL+"/api/agent/runs", string(encoded))
			if status != http.StatusAccepted {
				t.Fatalf("create failed: %d %s", status, body)
			}
			var created agentAPIRunResponse
			if err := json.Unmarshal(body, &created); err != nil {
				t.Fatal(err)
			}
			paused := waitForAgentAPIStatus(t, server.URL, created.ID, agentWaitingContinue)
			if len(paused.AvailableSkills) != 1 || len(paused.ActiveSkills) != 1 || paused.ActiveSkills[0].Name != "report" {
				t.Fatalf("API did not expose skill metadata: %#v", paused)
			}
			status, body = agentAPIRequest(t, http.MethodGet, server.URL+"/api/agent/runs/"+created.ID, "")
			if status != http.StatusOK || bytes.Contains(body, []byte("Separate verified facts from assumptions.")) {
				t.Fatalf("API should expose metadata, not skill bodies: %d %s", status, body)
			}
			if err := os.Remove(skillPath); err != nil {
				t.Fatal(err)
			}
			status, body = agentAPIRequest(t, http.MethodPost, server.URL+"/api/agent/runs/"+created.ID+"/continue", "")
			if status != http.StatusAccepted {
				t.Fatalf("continue failed: %d %s", status, body)
			}
			waitForAgentAPIStatus(t, server.URL, created.ID, agentWaitingApproval)
			if !criticSawSkill.Load() {
				t.Fatal("independent critic did not receive the original active skill after continuation")
			}
			status, body = agentAPIRequest(t, http.MethodPost, server.URL+"/api/agent/runs/"+created.ID+"/approve", "")
			if status != http.StatusOK {
				t.Fatalf("approve failed: %d %s", status, body)
			}
		})
	}
}

func TestAgentAPIContinueIterationBudget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	previousLimit, previousURL := agentIterationLimit, ollamaURL
	agentIterationLimit = 1
	t.Cleanup(func() { agentIterationLimit, ollamaURL = previousLimit, previousURL })
	ollama := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(response).Encode(ChatResponse{Message: ChatMessage{Content: `{"action":"update_findings","text":"working"}`}})
	}))
	defer ollama.Close()
	ollamaURL = ollama.URL
	api := newAgentAPI()
	server := httptest.NewServer(api.handler())
	defer server.Close()
	status, body := agentAPIRequest(t, http.MethodPost, server.URL+"/api/agent/runs", `{"goal":"prepare a result"}`)
	if status != http.StatusAccepted {
		t.Fatalf("create = %d: %s", status, body)
	}
	var created agentAPIRunResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	path := server.URL + "/api/agent/runs/" + created.ID
	for total := 1; total <= 2; total++ {
		run := waitForAgentAPIStatus(t, server.URL, created.ID, agentWaitingContinue)
		if run.Iteration != total || run.SegmentIteration != 1 || run.MaxIterations != 1 || !run.CompletedAt.IsZero() {
			t.Fatalf("unexpected paused counters: %+v", run)
		}
		api.evictExpiredRuns()
		assertAPIRunPresent(t, api, created.ID)
		for _, action := range []string{"approve", "feedback"} {
			status, _ = agentAPIRequest(t, http.MethodPost, path+"/"+action, `{"feedback":"continue"}`)
			if status != http.StatusConflict {
				t.Fatalf("%s should not continue a paused run: %d", action, status)
			}
		}
		if total == 1 {
			status, body = agentAPIRequest(t, http.MethodPost, path+"/continue", "")
			if status != http.StatusAccepted {
				t.Fatalf("continue = %d: %s", status, body)
			}
		}
	}
	status, body = agentAPIRequest(t, http.MethodPost, path+"/cancel", "")
	if status != http.StatusOK {
		t.Fatalf("cancel = %d: %s", status, body)
	}
	status, _ = agentAPIRequest(t, http.MethodPost, path+"/continue", "")
	if status != http.StatusConflict {
		t.Fatalf("continued terminal run: %d", status)
	}
}

func waitForAgentAPIStatus(t *testing.T, serverURL, runID string, expected AgentRunStatus) agentAPIRunResponse {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, body := agentAPIRequest(t, http.MethodGet, serverURL+"/api/agent/runs/"+runID, "")
		if status != http.StatusOK {
			t.Fatalf("get run status = %d, body = %s", status, body)
		}
		var run agentAPIRunResponse
		if err := json.Unmarshal(body, &run); err != nil {
			t.Fatalf("decode run response: %v", err)
		}
		if run.Status == expected {
			return run
		}
		if run.Status == agentFailed || run.Status == agentCancelled || run.Status == agentLimitReached {
			t.Fatalf("run ended with status %q: %s", run.Status, run.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run did not reach status %q", expected)
	return agentAPIRunResponse{}
}

func agentAPIRequest(t *testing.T, method, url, body string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, responseBody
}

func addTestAPIRun(api *agentAPI, id string, status AgentRunStatus, completedAt time.Time) {
	sess := &agentSession{ID: id, AgentRun: &AgentRun{ID: id, Status: status, CompletedAt: completedAt}}
	api.mu.Lock()
	api.runs[id] = &apiAgentRun{session: sess}
	api.mu.Unlock()
}

func assertAPIRunPresent(t *testing.T, api *agentAPI, id string) {
	t.Helper()
	api.mu.RLock()
	_, ok := api.runs[id]
	api.mu.RUnlock()
	if !ok {
		t.Fatalf("expected run %q to still be present", id)
	}
}

func assertAPIRunAbsent(t *testing.T, api *agentAPI, id string) {
	t.Helper()
	api.mu.RLock()
	_, ok := api.runs[id]
	api.mu.RUnlock()
	if ok {
		t.Fatalf("expected run %q to have been evicted", id)
	}
}

func withAgentAPIRetentionMinutes(t *testing.T, minutes int) {
	t.Helper()
	previous := agentAPIRetentionMinutes
	agentAPIRetentionMinutes = minutes
	t.Cleanup(func() { agentAPIRetentionMinutes = previous })
}

func TestAgentAPIEvictExpiredRunsRemovesOldTerminalRuns(t *testing.T) {
	withAgentAPIRetentionMinutes(t, 30)

	api := newAgentAPI()
	addTestAPIRun(api, "old-completed", agentCompleted, time.Now().UTC().Add(-2*time.Hour))
	addTestAPIRun(api, "recent-completed", agentCompleted, time.Now().UTC())
	addTestAPIRun(api, "still-running", agentRunning, time.Time{})
	addTestAPIRun(api, "waiting-approval", agentWaitingApproval, time.Time{})
	addTestAPIRun(api, "waiting-input", agentWaitingInput, time.Time{})

	api.evictExpiredRuns()

	assertAPIRunAbsent(t, api, "old-completed")
	assertAPIRunPresent(t, api, "recent-completed")
	assertAPIRunPresent(t, api, "still-running")
	assertAPIRunPresent(t, api, "waiting-approval")
	assertAPIRunPresent(t, api, "waiting-input")
}

func TestAgentAPIRetentionZeroDisablesEviction(t *testing.T) {
	withAgentAPIRetentionMinutes(t, 0)

	api := newAgentAPI()
	addTestAPIRun(api, "ancient-completed", agentCompleted, time.Now().UTC().Add(-48*time.Hour))

	api.evictExpiredRuns()

	assertAPIRunPresent(t, api, "ancient-completed")
}

func TestAgentAPIHandlerEvictsExpiredRunsBeforeLookup(t *testing.T) {
	withAgentAPIRetentionMinutes(t, 30)

	api := newAgentAPI()
	addTestAPIRun(api, "expired-run", agentCompleted, time.Now().UTC().Add(-2*time.Hour))

	server := httptest.NewServer(api.handler())
	defer server.Close()

	status, _ := agentAPIRequest(t, http.MethodGet, server.URL+"/api/agent/runs/expired-run", "")
	if status != http.StatusNotFound {
		t.Fatalf("expected the evicted run to 404 on lookup, got status %d", status)
	}
}
