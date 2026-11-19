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
	"time"
)

// fakeMasterNode simulates a recon API instance: a run stays "running" for one
// poll, then waits for approval; feedback re-runs it once.
type fakeMasterNode struct {
	mu       sync.Mutex
	token    string
	status   AgentRunStatus
	answer   string
	polls    int
	calls    []string
	feedback string
	hang     bool
}

func (f *fakeMasterNode) called(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func (f *fakeMasterNode) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if request.Header.Get("Authorization") != "Bearer "+f.token {
		writeAgentAPIError(response, http.StatusUnauthorized, "unauthorized")
		return
	}
	f.calls = append(f.calls, request.Method+" "+request.URL.Path)
	run := func() agentAPIRunResponse {
		r := agentAPIRunResponse{ID: "run_1", Status: f.status, Iteration: 2}
		r.Events = []AgentEvent{{Sequence: 1, Kind: "action", Action: "run_command", Command: "ping \x1b[31m-c 5 host"}}
		if f.status == agentWaitingApproval || f.status == agentCompleted {
			r.FinalAnswer = f.answer
		}
		return r
	}
	switch {
	case request.URL.Path == "/api/health":
		writeAgentAPIJSON(response, http.StatusOK, map[string]string{"status": "ok"})
	case request.Method == http.MethodPost && request.URL.Path == "/api/agent/runs":
		f.status = agentRunning
		writeAgentAPIJSON(response, http.StatusAccepted, run())
	case request.Method == http.MethodGet && request.URL.Path == "/api/agent/runs/run_1":
		f.polls++
		if f.status == agentRunning && f.polls >= 2 && !f.hang {
			f.status = agentWaitingApproval
		}
		writeAgentAPIJSON(response, http.StatusOK, run())
	case request.URL.Path == "/api/agent/runs/run_1/approve":
		f.status = agentCompleted
		writeAgentAPIJSON(response, http.StatusOK, run())
	case request.URL.Path == "/api/agent/runs/run_1/cancel":
		f.status = agentCancelled
		writeAgentAPIJSON(response, http.StatusOK, run())
	case request.URL.Path == "/api/agent/runs/run_1/feedback":
		var body agentAPIFeedbackRequest
		_ = json.NewDecoder(request.Body).Decode(&body)
		f.feedback = body.Feedback
		f.status = agentRunning
		f.polls = 0
		f.answer += " (revised)"
		writeAgentAPIJSON(response, http.StatusAccepted, run())
	default:
		writeAgentAPIError(response, http.StatusNotFound, "not found")
	}
}

func masterTestNodes(t *testing.T, token string, fakes ...*fakeMasterNode) []*masterNode {
	t.Helper()
	masterPollInterval = time.Millisecond
	var urls []string
	for _, fake := range fakes {
		fake.token = token
		server := httptest.NewServer(fake)
		t.Cleanup(server.Close)
		urls = append(urls, server.URL)
	}
	nodes, err := parseMasterNodes(strings.Join(urls, ","), token)
	if err != nil {
		t.Fatal(err)
	}
	return nodes
}

func TestParseMasterNodes(t *testing.T) {
	nodes, err := parseMasterNodes(" http://a:8080/ , https://b.example/recon,http://a:8080 ,", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].base != "http://a:8080" || nodes[1].base != "https://b.example/recon" || nodes[1].label != "b.example/recon" {
		t.Fatalf("unexpected nodes: %#v", nodes)
	}
	for _, bad := range []string{"", " , ", "a:8080", "ftp://a", "http://", "http://user:pw@a", "http://a?x=1", "http://a#f"} {
		if _, err := parseMasterNodes(bad, ""); err == nil {
			t.Fatalf("expected %q to be rejected", bad)
		}
	}
	var many []string
	for i := 0; i <= maxMasterNodes; i++ {
		many = append(many, "http://h"+string(rune('a'+i%26))+string(rune('a'+i/26))+":1")
	}
	if _, err := parseMasterNodes(strings.Join(many, ","), ""); err == nil {
		t.Fatal("expected node limit to be enforced")
	}
}

func TestRequireAPIToken(t *testing.T) {
	handler := requireAPIToken("secret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for header, want := range map[string]int{
		"":              http.StatusUnauthorized,
		"Bearer":        http.StatusUnauthorized,
		"Bearer ":       http.StatusUnauthorized,
		"Bearer wrong":  http.StatusUnauthorized,
		"secret":        http.StatusUnauthorized,
		"Bearer secret": http.StatusNoContent,
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		if header != "" {
			request.Header.Set("Authorization", header)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != want {
			t.Fatalf("Authorization %q: got %d, want %d", header, recorder.Code, want)
		}
	}
}

func TestMasterAggregatesNodesAndRelaysDecisions(t *testing.T) {
	approved := &fakeMasterNode{answer: "answer A"}
	revised := &fakeMasterNode{answer: "answer B"}
	nodes := masterTestNodes(t, "tok", approved, revised)
	deadServer := httptest.NewServer(http.NotFoundHandler())
	deadURL := deadServer.URL
	deadServer.Close()
	dead, err := parseMasterNodes(deadURL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	nodes = append(nodes, dead...)

	// Node order is deterministic: approve first node; revise then accept second.
	reader := bufio.NewReader(strings.NewReader("a\nr\nuse more detail\na\n"))
	var out bytes.Buffer
	if err := runMasterGoal(context.Background(), reader, nodes, "check connectivity", &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !approved.called("POST /api/agent/runs/run_1/approve") || !revised.called("POST /api/agent/runs/run_1/approve") {
		t.Fatalf("both nodes should be approved: %v %v", approved.calls, revised.calls)
	}
	if revised.feedback != "use more detail" {
		t.Fatalf("revision feedback not relayed: %q", revised.feedback)
	}
	for _, want := range []string{"answer A", "answer B (revised)", "completed", "rejected"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x1b[31m-c") || !strings.Contains(text, "ping [31m-c 5 host") {
		t.Fatalf("remote escape characters must be stripped:\n%q", text)
	}
	if nodes[2].status != masterNodeRejected {
		t.Fatalf("unreachable node should be rejected, got %q", nodes[2].status)
	}
}

func TestMasterRejectsWrongToken(t *testing.T) {
	fake := &fakeMasterNode{answer: "x"}
	nodes := masterTestNodes(t, "right", fake)
	nodes[0].token = "wrong"
	var out bytes.Buffer
	if err := runMasterGoal(context.Background(), bufio.NewReader(strings.NewReader("")), nodes, "goal", &out); err != nil {
		t.Fatal(err)
	}
	if nodes[0].status != masterNodeRejected || !strings.Contains(out.String(), "401") {
		t.Fatalf("expected a 401 rejection:\n%s", out.String())
	}
}

func TestMasterCancelsRemoteRunsOnInterrupt(t *testing.T) {
	fake := &fakeMasterNode{answer: "x", hang: true}
	nodes := masterTestNodes(t, "tok", fake)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for !fake.called("GET /api/agent/runs/run_1") {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	var out bytes.Buffer
	if err := runMasterGoal(ctx, bufio.NewReader(strings.NewReader("")), nodes, "goal", &out); err != nil {
		t.Fatal(err)
	}
	if !fake.called("POST /api/agent/runs/run_1/cancel") {
		t.Fatalf("interrupt must cancel remote runs: %v", fake.calls)
	}
}
