package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecuteBackgroundCommandLifecycle(t *testing.T) {
	job, err := executeBackgroundCommand("sess1", "run1", "echo hello")
	if err != nil {
		t.Fatalf("executeBackgroundCommand returned error: %v", err)
	}
	if job.RunID != "run1" || job.SessionID != "sess1" {
		t.Fatalf("expected job to carry sessionID/runID assigned at creation, got %#v", job)
	}
	select {
	case <-job.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not complete in time")
	}
	got, ok := jobStore.get(job.ID)
	if !ok {
		t.Fatal("expected job to be present in the store")
	}
	if got.Status != "completed" {
		t.Fatalf("expected status completed, got %q (error=%q)", got.Status, got.Error)
	}
	if got.RunID != "run1" {
		t.Fatalf("expected RunID to persist across completion, got %q", got.RunID)
	}
}

func TestExecuteBackgroundCommandCancelDoesNotGetOverwrittenAsFailed(t *testing.T) {
	job, err := executeBackgroundCommand("sess2", "run2", "sleep 5")
	if err != nil {
		t.Fatalf("executeBackgroundCommand returned error: %v", err)
	}
	if !jobStore.cancel(job.ID, "sess2") {
		t.Fatal("expected cancel to find the job")
	}
	select {
	case <-job.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled job did not finish in time")
	}
	got, ok := jobStore.get(job.ID)
	if !ok {
		t.Fatal("expected job to remain in the store after cancellation")
	}
	if got.Status != "cancelled" {
		t.Fatalf("expected killed process's natural exit to not overwrite the cancelled status, got %q", got.Status)
	}
}

func TestJobStoreCancelWrongSessionIsNoop(t *testing.T) {
	job, err := executeBackgroundCommand("owner", "run3", "sleep 5")
	if err != nil {
		t.Fatalf("executeBackgroundCommand returned error: %v", err)
	}
	t.Cleanup(func() {
		jobStore.cancel(job.ID, "owner")
		<-job.Done
	})
	if jobStore.cancel(job.ID, "someone-else") {
		t.Fatal("expected cancel to reject a session that does not own the job")
	}
	got, _ := jobStore.get(job.ID)
	if got.Status != "running" {
		t.Fatalf("expected job to remain running after a rejected cancel, got %q", got.Status)
	}
}

func TestJobStoreListBySessionFiltersBySession(t *testing.T) {
	jobA, err := executeBackgroundCommand("list-sess-a", "run-a", "echo a")
	if err != nil {
		t.Fatalf("executeBackgroundCommand returned error: %v", err)
	}
	jobB, err := executeBackgroundCommand("list-sess-b", "run-b", "echo b")
	if err != nil {
		t.Fatalf("executeBackgroundCommand returned error: %v", err)
	}
	<-jobA.Done
	<-jobB.Done

	jobs := jobStore.listBySession("list-sess-a")
	if len(jobs) != 1 || jobs[0].ID != jobA.ID {
		t.Fatalf("expected only session a's job, got %#v", jobs)
	}
}

func TestJobStoreConcurrentAccessIsRaceFree(t *testing.T) {
	job, err := executeBackgroundCommand("race-sess", "race-run", "sleep 1")
	if err != nil {
		t.Fatalf("executeBackgroundCommand returned error: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = jobStore.get(job.ID)
			_ = jobStore.listBySession("race-sess")
		}()
	}
	jobStore.cancel(job.ID, "race-sess")
	wg.Wait()
	<-job.Done
}

func TestClonePlanStepsIsDeepCopy(t *testing.T) {
	original := []AgentPlanStep{{ID: 1, Status: "pending", EvidenceEventIDs: []int{1, 2}}}
	cloned := clonePlanSteps(original)
	cloned[0].Status = "completed"
	cloned[0].EvidenceEventIDs[0] = 99
	if original[0].Status != "pending" {
		t.Fatalf("mutating the clone's status must not affect the original, got %q", original[0].Status)
	}
	if original[0].EvidenceEventIDs[0] != 1 {
		t.Fatalf("mutating the clone's evidence IDs must not affect the original, got %v", original[0].EvidenceEventIDs)
	}
}

func TestAgentSnapshotPlanIsIsolatedFromConcurrentMutation(t *testing.T) {
	sess := &agentSession{
		ID: "snap-sess",
		AgentRun: &AgentRun{
			ID:   "snap-run",
			Plan: []AgentPlanStep{{ID: 1, Status: "pending", EvidenceEventIDs: []int{1}}},
		},
	}
	snapshot := sess.agentSnapshot()

	sess.mu.Lock()
	sess.AgentRun.Plan[0].Status = "completed"
	sess.AgentRun.Plan[0].EvidenceEventIDs[0] = 42
	sess.mu.Unlock()

	if snapshot.Plan[0].Status != "pending" {
		t.Fatalf("snapshot must not observe a later mutation of the live plan, got %q", snapshot.Plan[0].Status)
	}
	if snapshot.Plan[0].EvidenceEventIDs[0] != 1 {
		t.Fatalf("snapshot's evidence IDs must not share the live plan's backing array, got %v", snapshot.Plan[0].EvidenceEventIDs)
	}
}

func TestIsRetryableOllamaTransportError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"429 too many requests", &ollamaHTTPStatusError{StatusCode: 429, Status: "429 Too Many Requests"}, true},
		{"500 internal server error", &ollamaHTTPStatusError{StatusCode: 500, Status: "500 Internal Server Error"}, true},
		{"503 service unavailable", &ollamaHTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}, true},
		{"400 bad request", &ollamaHTTPStatusError{StatusCode: 400, Status: "400 Bad Request"}, false},
		{"404 not found", &ollamaHTTPStatusError{StatusCode: 404, Status: "404 Not Found"}, false},
		{"network error", &url.Error{Op: "Post", URL: "http://x", Err: errors.New("connection refused")}, true},
		{"generic non-transport error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableOllamaTransportError(tc.err); got != tc.want {
				t.Fatalf("isRetryableOllamaTransportError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func withTestOllamaServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	previousURL := ollamaURL
	ollamaURL = server.URL
	t.Cleanup(func() { ollamaURL = previousURL })
}

func withFastRetryBackoff(t *testing.T, backoff time.Duration) {
	t.Helper()
	previous := ollamaRetryBaseBackoff
	ollamaRetryBaseBackoff = backoff
	t.Cleanup(func() { ollamaRetryBaseBackoff = previous })
}

func TestOllamaChatOptionsDefaultsToUnlimitedNumPredict(t *testing.T) {
	if got := ollamaChatOptions().NumPredict; got != ollamaNumPredict {
		t.Fatalf("expected NumPredict to reflect ollamaNumPredict (%d), got %d", ollamaNumPredict, got)
	}
	if ollamaNumPredict != -1 {
		t.Fatalf("expected default OLLAMA_NUM_PREDICT of -1 (no limit) when unset, got %d", ollamaNumPredict)
	}
}

func TestCallOllamaMessageContextSendsNumPredict(t *testing.T) {
	var captured ChatRequest
	withTestOllamaServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: "ok"}})
	})

	if _, err := callOllamaMessageContext(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, false, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured.Options == nil || captured.Options.NumPredict != ollamaNumPredict {
		t.Fatalf("expected request options to carry NumPredict=%d, got %#v", ollamaNumPredict, captured.Options)
	}
}

func TestCallOllamaMessageWithRetrySucceedsAfterTransientServerError(t *testing.T) {
	withFastRetryBackoff(t, time.Millisecond)
	var calls int32
	withTestOllamaServer(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: "ok"}})
	})

	message, err := callOllamaMessageWithRetry(context.Background(), "test", []ChatMessage{{Role: "user", Content: "hi"}}, false, false)
	if err != nil {
		t.Fatalf("expected retry to succeed, got error: %v", err)
	}
	if message.Content != "ok" {
		t.Fatalf("unexpected message: %#v", message)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly 2 attempts (1 failure + 1 success), got %d", got)
	}
}

func TestCallOllamaMessageWithRetryDoesNotRetryDeterministic4xx(t *testing.T) {
	var calls int32
	withTestOllamaServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	})

	if _, err := callOllamaMessageWithRetry(context.Background(), "test", []ChatMessage{{Role: "user", Content: "hi"}}, false, false); err == nil {
		t.Fatal("expected an error for a deterministic 400 response")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 attempt for a non-retryable error, got %d", got)
	}
}

func TestCallOllamaMessageWithRetryExhaustsAttemptsOnPersistentFailure(t *testing.T) {
	withFastRetryBackoff(t, time.Millisecond)
	var calls int32
	withTestOllamaServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	if _, err := callOllamaMessageWithRetry(context.Background(), "test", []ChatMessage{{Role: "user", Content: "hi"}}, false, false); err == nil {
		t.Fatal("expected an error after exhausting all retry attempts")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected exactly 3 attempts (max), got %d", got)
	}
}

func TestCallOllamaMessageWithRetryStopsAfterCancellation(t *testing.T) {
	withFastRetryBackoff(t, 200*time.Millisecond)
	var calls int32
	withTestOllamaServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := callOllamaMessageWithRetry(ctx, "test", []ChatMessage{{Role: "user", Content: "hi"}}, false, false); err == nil {
		t.Fatal("expected an error once the context is cancelled")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 attempt before cancellation interrupted the backoff, got %d", got)
	}
}

