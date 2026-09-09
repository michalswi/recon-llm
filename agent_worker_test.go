package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestListAgentDirectoryRespectsDepthAndLists(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "sub")
	if err := os.Mkdir(nested, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "b.txt"), []byte("b"), 0600); err != nil {
		t.Fatal(err)
	}
	deeper := filepath.Join(nested, "deeper")
	if err := os.Mkdir(deeper, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deeper, "c.txt"), []byte("c"), 0600); err != nil {
		t.Fatal(err)
	}

	output, success := listAgentDirectory(root, 1)
	if !success {
		t.Fatalf("listAgentDirectory failed: %s", output)
	}
	if !strings.Contains(output, "a.txt") || !strings.Contains(output, "sub") {
		t.Fatalf("expected depth-1 entries, got: %s", output)
	}
	if strings.Contains(output, "b.txt") {
		t.Fatalf("depth limit was not respected, got: %s", output)
	}

	output, success = listAgentDirectory(root, 3)
	if !success {
		t.Fatalf("listAgentDirectory failed: %s", output)
	}
	if !strings.Contains(output, "b.txt") || !strings.Contains(output, "c.txt") {
		t.Fatalf("expected nested entries at depth 3, got: %s", output)
	}
}

func TestListAgentDirectoryRejectsFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir.txt")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, success := listAgentDirectory(file, 1); success {
		t.Fatal("listAgentDirectory should fail for a non-directory path")
	}
}

func TestSearchAgentFilesFindsMatchesAcrossDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.txt"), []byte("hello world\nfoo bar\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "two.txt"), []byte("nothing here\n"), 0600); err != nil {
		t.Fatal(err)
	}

	output, success := searchAgentFiles(root, "^hello")
	if !success {
		t.Fatalf("searchAgentFiles failed: %s", output)
	}
	if !strings.Contains(output, "one.txt:1:") || strings.Contains(output, "two.txt") {
		t.Fatalf("unexpected search output: %s", output)
	}
}

func TestSearchAgentFilesRejectsInvalidPattern(t *testing.T) {
	if _, err := parseAgentAction(`{"action":"search_files","path":"/tmp","pattern":"("}`); err == nil {
		t.Fatal("expected parseAgentAction to reject an invalid regex pattern")
	}
}

func TestParseAgentActionValidatesListDirectoryAndSearchFiles(t *testing.T) {
	if _, err := parseAgentAction(`{"action":"list_directory"}`); err == nil {
		t.Fatal("expected list_directory to require path")
	}
	action, err := parseAgentAction(`{"action":"list_directory","path":"/tmp","maxDepth":2}`)
	if err != nil {
		t.Fatalf("parseAgentAction returned error: %v", err)
	}
	if action.Path != "/tmp" || action.MaxDepth != 2 {
		t.Fatalf("unexpected parsed action: %#v", action)
	}
	if _, err := parseAgentAction(`{"action":"search_files","path":"/tmp"}`); err == nil {
		t.Fatal("expected search_files to require pattern")
	}
}

func TestAgentPlanEvidenceEventIDsDedupesCompletedSteps(t *testing.T) {
	plan := []AgentPlanStep{
		{ID: 1, Status: "completed", EvidenceEventIDs: []int{3, 1}},
		{ID: 2, Status: "pending", EvidenceEventIDs: []int{5}},
		{ID: 3, Status: "completed", EvidenceEventIDs: []int{1, 2}},
	}
	if got := agentPlanEvidenceEventIDs(plan); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("expected deduped sorted [1 2 3] from completed steps only, got %v", got)
	}
}

func TestAgentEvidenceLedgerPullsCompactedEvidence(t *testing.T) {
	run := &AgentRun{
		Events: []AgentEvent{
			{Sequence: 1, Kind: "observation", Text: "old fact"},
			{Sequence: 2, Kind: "observation", Text: "recent fact"},
		},
		CompactedThrough: 1,
		Plan:             []AgentPlanStep{{ID: 1, Status: "completed", EvidenceEventIDs: []int{1}}},
	}
	windowed := selectAgentContextEvents(run.Events, run.CompactedThrough, agentContextTokens)
	if len(windowed) != 1 || windowed[0].Sequence != 2 {
		t.Fatalf("expected only event 2 in the recent window, got %#v", windowed)
	}
	ledger := agentEvidenceLedger(run, windowed)
	if len(ledger) != 1 || ledger[0].Sequence != 1 {
		t.Fatalf("expected compacted event 1 pulled into the ledger, got %#v", ledger)
	}
}

func TestAgentEvidenceLedgerSkipsEventsAlreadyInWindow(t *testing.T) {
	run := &AgentRun{
		Events: []AgentEvent{{Sequence: 1, Kind: "observation", Text: "fact"}},
		Plan:   []AgentPlanStep{{ID: 1, Status: "completed", EvidenceEventIDs: []int{1}}},
	}
	windowed := selectAgentContextEvents(run.Events, run.CompactedThrough, agentContextTokens)
	if ledger := agentEvidenceLedger(run, windowed); len(ledger) != 0 {
		t.Fatalf("expected no duplicate ledger entries for evidence already in window, got %#v", ledger)
	}
}

func TestMergeAgentSummaryFallbackKeepsNewestFacts(t *testing.T) {
	previous := strings.Repeat("old", 20000)
	newest := "critical newly compacted fact"
	merged := mergeAgentSummaryFallback(previous, newest)
	if !strings.HasSuffix(merged, newest) {
		tail := merged
		if len(tail) > 60 {
			tail = tail[len(tail)-60:]
		}
		t.Fatalf("expected merged summary to retain the newest facts, got tail: %q", tail)
	}
	if len(merged) > agentMaxObservation {
		t.Fatalf("merged summary exceeds observation cap: %d bytes", len(merged))
	}
}

func TestMergeAgentSummaryFallbackOversizedNewestIsNotDropped(t *testing.T) {
	newest := strings.Repeat("x", agentMaxObservation+100)
	merged := mergeAgentSummaryFallback("previous summary", newest)
	if !strings.Contains(merged, "xxxx") {
		t.Fatalf("expected oversized newest content to still be represented, got len=%d", len(merged))
	}
}

func TestVerifyAgentAnswerIncludesSummaryAndCompactedLedgerEvidence(t *testing.T) {
	var captured ChatRequest
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode critic request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: `{"approved":true,"feedback":"","evidenceEventIds":[]}`}})
	}))
	defer ollama.Close()
	previousOllamaURL := ollamaURL
	ollamaURL = ollama.URL
	t.Cleanup(func() { ollamaURL = previousOllamaURL })

	sess := &agentSession{
		ID: "sess-critic",
		AgentRun: &AgentRun{
			ID:               "run-critic",
			GoalSpec:         normalizeAgentGoalSpec(AgentGoalSpec{Objective: "produce a report"}),
			ContextSummary:   "earlier finding: host is up",
			CompactedThrough: 1,
			Events: []AgentEvent{
				{Sequence: 1, Kind: "observation", Text: "ping result", Success: true},
				{Sequence: 2, Kind: "observation", Text: "recent event", Success: true},
			},
			Plan: []AgentPlanStep{{ID: 1, Status: "completed", EvidenceEventIDs: []int{1}}},
		},
	}

	if _, err := verifyAgentAnswer(context.Background(), sess, "final report text"); err != nil {
		t.Fatalf("verifyAgentAnswer returned error: %v", err)
	}
	if len(captured.Messages) < 2 {
		t.Fatalf("expected system+user messages sent to critic, got %d", len(captured.Messages))
	}
	userContent := captured.Messages[1].Content
	if !strings.Contains(userContent, "earlier finding: host is up") {
		t.Fatalf("expected critic message to include the earlier run summary, got: %s", userContent)
	}
	if !strings.Contains(userContent, "ping result") {
		t.Fatalf("expected critic message to include compacted plan-step evidence, got: %s", userContent)
	}
}

