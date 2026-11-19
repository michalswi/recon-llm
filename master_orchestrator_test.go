package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeOllama replays scripted orchestrator actions and critic verdicts.
type fakeOllama struct {
	mu           sync.Mutex
	actions      []string
	critic       []string
	next         int
	criticNext   int
	systems      []string
	criticInputs []string
}

func (f *fakeOllama) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	var chat ChatRequest
	_ = json.NewDecoder(request.Body).Decode(&chat)
	f.mu.Lock()
	defer f.mu.Unlock()
	content := "not json"
	if strings.HasPrefix(chat.Messages[0].Content, "You are an independent critic") {
		f.criticInputs = append(f.criticInputs, chat.Messages[1].Content)
		if f.criticNext < len(f.critic) {
			content = f.critic[f.criticNext]
			f.criticNext++
		} else {
			content = `{"approved":true}`
		}
	} else {
		f.systems = append(f.systems, chat.Messages[0].Content)
		if f.next < len(f.actions) {
			content = f.actions[f.next]
			f.next++
		}
	}
	writeAgentAPIJSON(response, http.StatusOK, ChatResponse{Message: ChatMessage{Role: "assistant", Content: content}})
}

func useFakeOllama(t *testing.T, fake *fakeOllama) {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	previousURL, previousRetry := ollamaURL, ollamaRetryBaseBackoff
	ollamaURL, ollamaRetryBaseBackoff = server.URL, 0
	t.Cleanup(func() { ollamaURL, ollamaRetryBaseBackoff = previousURL, previousRetry })
}

func runOrchestrator(t *testing.T, fake *fakeOllama, input string, nodes []*masterNode) (string, error) {
	t.Helper()
	useFakeOllama(t, fake)
	var out bytes.Buffer
	err := orchestrateMaster(context.Background(), bufio.NewReader(strings.NewReader(input)), nodes, "compare the two hosts", &out)
	return out.String(), err
}

func TestParseMasterAction(t *testing.T) {
	valid := []string{
		`{"action":"list_nodes"}`,
		`{"action":"dispatch_run","node":"n1","goal":"ping host","constraints":"five pings"}`,
		`{"action":"check_run","runId":"r1"}`,
		`{"action":"respond_run","runId":"r1","decision":"accept"}`,
		`{"action":"respond_run","runId":"r1","decision":"feedback","text":"add RTT"}`,
		`{"action":"answer","text":"done"}`,
	}
	for _, raw := range valid {
		if _, err := parseMasterAction(raw); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	invalid := []string{
		``, `not json`, `{"action":"list_nodes"} {"action":"list_nodes"}`,
		`{"action":"explode"}`,
		`{"action":"list_nodes","node":"n1"}`,
		`{"action":"dispatch_run","node":"n1"}`,
		`{"action":"dispatch_run","goal":"x"}`,
		`{"action":"check_run"}`,
		`{"action":"check_run","runId":"r1","goal":"x"}`,
		`{"action":"respond_run","runId":"r1","decision":"feedback"}`,
		`{"action":"respond_run","runId":"r1","decision":"accept","text":"x"}`,
		`{"action":"respond_run","runId":"r1","decision":"destroy"}`,
		`{"action":"answer","text":"x","unknown":1}`,
		`{"action":"answer"}`,
	}
	for _, raw := range invalid {
		if _, err := parseMasterAction(raw); err == nil {
			t.Fatalf("expected %q to be rejected", raw)
		}
	}
}

func TestOrchestratorDispatchesReviewsAndMerges(t *testing.T) {
	a := &fakeMasterNode{answer: "answer A"}
	b := &fakeMasterNode{answer: "answer B"}
	nodes := masterTestNodes(t, "tok", a, b)
	fake := &fakeOllama{actions: []string{
		`{"action":"list_nodes"}`,
		`{"action":"dispatch_run","node":"n1","goal":"inspect host A"}`,
		`{"action":"dispatch_run","node":"n2","goal":"inspect host B"}`,
		`{"action":"check_run","runId":"r1"}`,
		`{"action":"check_run","runId":"r2"}`,
		`{"action":"respond_run","runId":"r1","decision":"accept"}`,
		`{"action":"respond_run","runId":"r2","decision":"feedback","text":"more detail"}`,
		`{"action":"check_run","runId":"r2"}`,
		`{"action":"respond_run","runId":"r2","decision":"accept"}`,
		`{"action":"answer","text":"A says answer A; B says answer B (revised)"}`,
	}}
	// "a" allows all dispatches; the final "a" accepts the merged answer.
	out, err := runOrchestrator(t, fake, "a\na\n", nodes)
	if err != nil {
		t.Fatal(err)
	}
	if !a.called("POST /api/agent/runs/run_1/approve") || !b.called("POST /api/agent/runs/run_1/approve") {
		t.Fatalf("both runs should be accepted: %v %v", a.calls, b.calls)
	}
	if b.feedback != "more detail" {
		t.Fatalf("revision feedback not relayed: %q", b.feedback)
	}
	if !strings.Contains(out, "Orchestrator result accepted") {
		t.Fatalf("final answer was not accepted:\n%s", out)
	}
	last := fake.systems[len(fake.systems)-1]
	if !strings.Contains(last, "accepted result: answer A") || !strings.Contains(last, "accepted result: answer B (revised)") {
		t.Fatalf("registry should carry accepted results:\n%s", last)
	}
	if len(fake.criticInputs) != 1 || !strings.Contains(fake.criticInputs[0], "answer B (revised)") {
		t.Fatalf("critic must see node results: %v", fake.criticInputs)
	}
	if a.called("POST /api/agent/runs/run_1/cancel") || b.called("POST /api/agent/runs/run_1/cancel") {
		t.Fatal("accepted runs must not be cancelled")
	}
}

func TestOrchestratorDeclinedDispatchIsNotSent(t *testing.T) {
	a := &fakeMasterNode{answer: "answer A"}
	nodes := masterTestNodes(t, "tok", a)
	fake := &fakeOllama{actions: []string{
		`{"action":"dispatch_run","node":"n1","goal":"inspect host A"}`,
		`{"action":"answer","text":"nothing was run"}`,
	}}
	if _, err := runOrchestrator(t, fake, "n\ns\n", nodes); err != nil {
		t.Fatal(err)
	}
	if a.called("POST /api/agent/runs") {
		t.Fatalf("a declined dispatch must not reach the node: %v", a.calls)
	}
	if got := fake.systems[len(fake.systems)-1]; !strings.Contains(got, "No runs dispatched yet.") {
		t.Fatalf("registry should stay empty:\n%s", got)
	}
}

func TestOrchestratorCriticRejectionAndCleanup(t *testing.T) {
	a := &fakeMasterNode{answer: "answer A"}
	nodes := masterTestNodes(t, "tok", a)
	fake := &fakeOllama{
		actions: []string{
			`{"action":"dispatch_run","node":"n1","goal":"inspect host A"}`,
			`{"action":"check_run","runId":"r1"}`,
			`{"action":"answer","text":"made up answer"}`,
			`{"action":"answer","text":"answer A"}`,
		},
		critic: []string{`{"approved":false,"feedback":"claims not supported"}`, `{"approved":true}`},
	}
	out, err := runOrchestrator(t, fake, "y\na\n", nodes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "claims not supported") || !strings.Contains(out, "Orchestrator result accepted") {
		t.Fatalf("critic rejection then approval expected:\n%s", out)
	}
	if !a.called("POST /api/agent/runs/run_1/cancel") {
		t.Fatalf("an unaccepted run must be cancelled at the end: %v", a.calls)
	}
}

func TestOrchestratorStopsAfterRepeatedInvalidActions(t *testing.T) {
	nodes := masterTestNodes(t, "tok", &fakeMasterNode{})
	_, err := runOrchestrator(t, &fakeOllama{}, "", nodes)
	if err == nil || !strings.Contains(err.Error(), "invalid actions") {
		t.Fatalf("expected an invalid-action failure, got %v", err)
	}
}
