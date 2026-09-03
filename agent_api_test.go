package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
