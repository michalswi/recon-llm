package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestAgentEvidenceEventIsValid(t *testing.T) {
	cases := []struct {
		name  string
		event AgentEvent
		want  bool
	}{
		{"successful run_command", AgentEvent{Kind: "observation", Action: "run_command", Success: true}, true},
		{"failed run_command", AgentEvent{Kind: "observation", Action: "run_command", Success: false}, false},
		{"successful list_directory", AgentEvent{Kind: "observation", Action: "list_directory", Success: true}, true},
		{"failed search_files", AgentEvent{Kind: "observation", Action: "search_files", Success: false}, false},
		{"successful write_file", AgentEvent{Kind: "observation", Action: "write_file", Success: true}, true},
		{"failed write_file", AgentEvent{Kind: "observation", Action: "write_file", Success: false}, false},
		{"run_command_bg start", AgentEvent{Kind: "observation", Action: "run_command_bg", Success: false}, true},
		{"get_job completed", AgentEvent{Kind: "observation", Action: "get_job", Text: "job job_1 status: completed\n"}, true},
		{"check_job running", AgentEvent{Kind: "observation", Action: "check_job", Text: "job job_1 status: running\n"}, false},
		{"cancel_job", AgentEvent{Kind: "observation", Action: "cancel_job"}, true},
		{"list_jobs", AgentEvent{Kind: "observation", Action: "list_jobs"}, true},
		{"update_findings", AgentEvent{Kind: "observation", Action: "update_findings"}, true},
		{"feedback", AgentEvent{Kind: "feedback"}, true},
		{"plan", AgentEvent{Kind: "plan"}, true},
		{"action (stated intent)", AgentEvent{Kind: "action", Action: "run_command"}, false},
		{"invalid_action", AgentEvent{Kind: "invalid_action"}, false},
		{"candidate", AgentEvent{Kind: "candidate"}, false},
		{"answer", AgentEvent{Kind: "answer"}, false},
		{"verification", AgentEvent{Kind: "verification"}, false},
		{"clarification", AgentEvent{Kind: "clarification"}, false},
		{"status", AgentEvent{Kind: "status"}, false},
		{"error", AgentEvent{Kind: "error"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentEvidenceEventIsValid(tc.event); got != tc.want {
				t.Fatalf("agentEvidenceEventIsValid(%#v) = %v, want %v", tc.event, got, tc.want)
			}
		})
	}
}

func TestParseAgentActionWriteFile(t *testing.T) {
	action, err := parseAgentAction(`{"action":"write_file","path":"/tmp/main.go","content":"package main\n\nfunc main() {}\n"}`)
	if err != nil {
		t.Fatalf("parseAgentAction returned error: %v", err)
	}
	if action.Content == nil || *action.Content != "package main\n\nfunc main() {}\n" {
		t.Fatalf("unexpected write content: %#v", action.Content)
	}
	if _, err := parseAgentAction(`{"action":"write_file","path":"/tmp/empty","content":"","overwrite":false}`); err != nil {
		t.Fatalf("empty file content should be accepted: %v", err)
	}
	for _, raw := range []string{
		`{"action":"write_file","content":"x"}`,
		`{"action":"write_file","path":"/tmp/x"}`,
		`{"action":"answer","text":"done","content":"x"}`,
		`{"action":"answer","text":"done","overwrite":false}`,
	} {
		if _, err := parseAgentAction(raw); err == nil {
			t.Fatalf("expected invalid write action to be rejected: %s", raw)
		}
	}
	oversized, _ := json.Marshal(ToolResponse{Action: "write_file", Path: "/tmp/large", Content: stringPointer(strings.Repeat("x", agentMaxWriteSize+1))})
	if _, err := parseAgentAction(string(oversized)); err == nil {
		t.Fatal("expected oversized write content to be rejected")
	}
}

func stringPointer(value string) *string {
	return &value
}

func TestWriteAgentFileConfinementAndOverwrite(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	outside, _ = filepath.EvalSymlinks(outside)
	sess := &agentSession{ID: "write-session", AgentRun: &AgentRun{ID: "write-run", WritableRoots: []string{root}}}
	content := "package main\n\nfunc main() {}\n"
	path := filepath.Join(root, "main.go")

	resolved, bytesWritten, err := writeAgentFile(sess, ToolResponse{Action: "write_file", Path: path, Content: &content})
	if err != nil {
		t.Fatalf("writeAgentFile returned error: %v", err)
	}
	if resolved != path || bytesWritten != len(content) {
		t.Fatalf("write result = (%q, %d), want (%q, %d)", resolved, bytesWritten, path, len(content))
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != content {
		t.Fatalf("written file = %q, %v", data, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("written mode = %v, %v", info.Mode().Perm(), err)
	}

	replacement := "replacement\n"
	if _, _, err := writeAgentFile(sess, ToolResponse{Action: "write_file", Path: path, Content: &replacement}); err == nil {
		t.Fatal("expected overwrite without explicit authorization to fail")
	}
	overwrite := true
	if _, _, err := writeAgentFile(sess, ToolResponse{Action: "write_file", Path: path, Content: &replacement, Overwrite: &overwrite}); err != nil {
		t.Fatalf("explicit overwrite failed: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != replacement {
		t.Fatalf("overwritten file = %q", data)
	}

	escape := filepath.Join(outside, "escape.go")
	if _, _, err := writeAgentFile(sess, ToolResponse{Action: "write_file", Path: escape, Content: &content}); err == nil {
		t.Fatal("expected write outside authorized root to fail")
	}
	parentLink := filepath.Join(root, "outside-link")
	if err := os.Symlink(outside, parentLink); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeAgentFile(sess, ToolResponse{Action: "write_file", Path: filepath.Join(parentLink, "escape.go"), Content: &content}); err == nil {
		t.Fatal("expected symlink parent escape to fail")
	}
	target := filepath.Join(outside, "target.go")
	if err := os.WriteFile(target, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	destinationLink := filepath.Join(root, "destination-link.go")
	if err := os.Symlink(target, destinationLink); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeAgentFile(sess, ToolResponse{Action: "write_file", Path: destinationLink, Content: &content, Overwrite: &overwrite}); err == nil {
		t.Fatal("expected symlink destination to fail")
	}
	if data, _ := os.ReadFile(target); string(data) != "safe" {
		t.Fatalf("symlink target was modified: %q", data)
	}
	if temporary, _ := filepath.Glob(filepath.Join(root, ".recon-write-*")); len(temporary) != 0 {
		t.Fatalf("temporary write files were not cleaned up: %v", temporary)
	}
}

func TestExecuteWriteFileRecordsMetadataAndLimits(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	sess := &agentSession{ID: "write-events", AgentRun: &AgentRun{ID: "write-events-run", WritableRoots: []string{root}}}
	content := "line one\nline two\n"
	path := filepath.Join(root, "notes.txt")
	if !executeAgentAction(context.Background(), sess, ToolResponse{Action: "write_file", Path: path, Content: &content}) {
		t.Fatal("write_file stopped agent execution")
	}
	run := sess.agentSnapshot()
	if len(run.Events) != 1 || !run.Events[0].Success || run.Events[0].Path != path || run.Events[0].BytesWritten != len(content) {
		t.Fatalf("unexpected write observation: %#v", run.Events)
	}
	encoded, err := json.Marshal(run.Events[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(content)) {
		t.Fatalf("write event leaked file content: %s", encoded)
	}
	if run.WrittenFiles != 1 || run.WrittenBytes != len(content) {
		t.Fatalf("unexpected write counters: files=%d bytes=%d", run.WrittenFiles, run.WrittenBytes)
	}
	if criticContent := agentWrittenFileContents(run.Events); !strings.Contains(criticContent, strings.TrimSpace(content)) {
		t.Fatalf("transient critic evidence missing file content: %q", criticContent)
	}

	sess.AgentRun.WrittenFiles = agentMaxWriteFiles
	second := filepath.Join(root, "second.txt")
	if !executeAgentAction(context.Background(), sess, ToolResponse{Action: "write_file", Path: second, Content: &content}) {
		t.Fatal("failed write should remain recoverable")
	}
	last := sess.agentSnapshot().Events[1]
	if last.Success || !strings.Contains(last.Text, "limit exceeded") {
		t.Fatalf("expected failed limit observation, got %#v", last)
	}
	if _, err := os.Stat(second); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("limited write created a destination: %v", err)
	}
	sess.AgentRun.WrittenFiles = 0
	sess.AgentRun.WrittenBytes = agentMaxWriteBytes
	third := filepath.Join(root, "third.txt")
	if !executeAgentAction(context.Background(), sess, ToolResponse{Action: "write_file", Path: third, Content: &content}) {
		t.Fatal("failed byte-limited write should remain recoverable")
	}
	last = sess.agentSnapshot().Events[2]
	if last.Success || !strings.Contains(last.Text, "limit exceeded") {
		t.Fatalf("expected failed byte-limit observation, got %#v", last)
	}
}

func TestCommandObservationSupportsRequiredCommand(t *testing.T) {
	cases := []struct {
		name  string
		event AgentEvent
		want  bool
	}{
		{"successful foreground command", AgentEvent{Kind: "observation", Action: "run_command", Command: "ping -c 1 localhost", Success: true}, true},
		{"completed check job", AgentEvent{Kind: "observation", Action: "check_job", Command: "ping -c 60 localhost", Success: true}, true},
		{"completed get job", AgentEvent{Kind: "observation", Action: "get_job", Command: "ping -c 60 localhost", Success: true}, true},
		{"failed foreground command", AgentEvent{Kind: "observation", Action: "run_command", Command: "ping bad", Success: false}, false},
		{"failed background command", AgentEvent{Kind: "observation", Action: "check_job", Command: "ping bad", Success: false}, false},
		{"cancelled background command", AgentEvent{Kind: "observation", Action: "get_job", Command: "ping localhost", Success: false}, false},
		{"timed out background command", AgentEvent{Kind: "observation", Action: "check_job", Command: "ping localhost", Success: false}, false},
		{"different completed command", AgentEvent{Kind: "observation", Action: "check_job", Command: "echo done", Success: true}, false},
		{"background start only", AgentEvent{Kind: "observation", Action: "run_command_bg", Command: "ping localhost", Success: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.event.Sequence = 1
			events := []AgentEvent{tc.event}
			if got := hasCommandObservation(events, "ping"); got != tc.want {
				t.Fatalf("hasCommandObservation() = %v, want %v", got, tc.want)
			}
			if got := evidenceSupportsCommand(events, []int{1}, "ping"); got != tc.want {
				t.Fatalf("evidenceSupportsCommand() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateAgentActionStateRejectsInvalidCompleteStepEvidence(t *testing.T) {
	sess := &agentSession{ID: "sess1"}
	run := &AgentRun{
		ID: "run1",
		Events: []AgentEvent{
			{Sequence: 1, Kind: "observation", Action: "run_command", Success: false},
		},
		Plan: []AgentPlanStep{{ID: 1, Status: "pending"}},
	}
	action := ToolResponse{Action: "complete_step", StepID: 1, EvidenceEventIDs: []int{1}}
	if err := validateAgentActionState(sess, run, action); err == nil {
		t.Fatal("expected complete_step citing a failed command observation to be rejected")
	}
}

func TestValidateAgentActionStateAcceptsValidCompleteStepEvidence(t *testing.T) {
	sess := &agentSession{ID: "sess1"}
	run := &AgentRun{
		ID: "run1",
		Events: []AgentEvent{
			{Sequence: 1, Kind: "observation", Action: "run_command", Success: true},
		},
		Plan: []AgentPlanStep{{ID: 1, Status: "pending"}},
	}
	action := ToolResponse{Action: "complete_step", StepID: 1, EvidenceEventIDs: []int{1}}
	if err := validateAgentActionState(sess, run, action); err != nil {
		t.Fatalf("expected complete_step citing a successful observation to be accepted, got: %v", err)
	}
}

func TestVerifyAgentAnswerApprovesDespitePendingPlanStep(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: `{"approved":true,"feedback":"","evidenceEventIds":[1,2]}`}})
	}))
	defer ollama.Close()
	previousOllamaURL := ollamaURL
	ollamaURL = ollama.URL
	t.Cleanup(func() { ollamaURL = previousOllamaURL })

	run := &AgentRun{
		ID:       "run-pending-plan",
		GoalSpec: normalizeAgentGoalSpec(AgentGoalSpec{Objective: "verify if lamos.space website is up"}),
		Events: []AgentEvent{
			{Sequence: 1, Kind: "observation", Action: "run_command", Text: "dig lamos.space", Success: true},
			{Sequence: 2, Kind: "observation", Action: "run_command", Text: "curl -I http://lamos.space", Success: true},
		},
		Plan: []AgentPlanStep{
			{ID: 1, Status: "pending"},
			{ID: 2, Status: "pending"},
		},
	}
	sess := &agentSession{ID: "sess-pending-plan", AgentRun: run}

	verification, err := verifyAgentAnswer(context.Background(), sess, "lamos.space resolves and responds with HTTP 200")
	if err != nil {
		t.Fatalf("verifyAgentAnswer returned error: %v", err)
	}
	if !verification.Approved {
		t.Fatalf("expected critic approval to survive pending plan steps, got feedback: %q", verification.Feedback)
	}
	if run.Plan[0].Status != "pending" || run.Plan[1].Status != "pending" {
		t.Fatalf("expected pending plan steps to remain unchanged for diagnostics, got: %#v", run.Plan)
	}
}

func TestVerifyAgentAnswerCriticRejectionPreservedWithPendingPlan(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: `{"approved":false,"feedback":"the HTTP check was never performed","evidenceEventIds":[]}`}})
	}))
	defer ollama.Close()
	previousOllamaURL := ollamaURL
	ollamaURL = ollama.URL
	t.Cleanup(func() { ollamaURL = previousOllamaURL })

	sess := &agentSession{
		ID: "sess-pending-plan-rejected",
		AgentRun: &AgentRun{
			ID:       "run-pending-plan-rejected",
			GoalSpec: normalizeAgentGoalSpec(AgentGoalSpec{Objective: "verify if lamos.space website is up"}),
			Events: []AgentEvent{
				{Sequence: 1, Kind: "observation", Action: "run_command", Text: "dig lamos.space", Success: true},
			},
			Plan: []AgentPlanStep{{ID: 1, Status: "pending"}, {ID: 2, Status: "pending"}},
		},
	}

	verification, err := verifyAgentAnswer(context.Background(), sess, "lamos.space resolves")
	if err != nil {
		t.Fatalf("verifyAgentAnswer returned error: %v", err)
	}
	if verification.Approved {
		t.Fatal("expected critic rejection of genuinely unfinished work to still be respected")
	}
	if verification.Feedback != "the HTTP check was never performed" {
		t.Fatalf("expected original critic feedback to be preserved, got: %q", verification.Feedback)
	}
}

func TestVerifyAgentAnswerRejectsCriticEvidenceThatCannotProveIt(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: `{"approved":true,"feedback":"","evidenceEventIds":[1]}`}})
	}))
	defer ollama.Close()
	previousOllamaURL := ollamaURL
	ollamaURL = ollama.URL
	t.Cleanup(func() { ollamaURL = previousOllamaURL })

	sess := &agentSession{
		ID: "sess-critic-invalid",
		AgentRun: &AgentRun{
			ID:       "run-critic-invalid",
			GoalSpec: normalizeAgentGoalSpec(AgentGoalSpec{Objective: "produce a report"}),
			Events: []AgentEvent{
				{Sequence: 1, Kind: "observation", Action: "run_command", Success: false},
			},
		},
	}

	verification, err := verifyAgentAnswer(context.Background(), sess, "final report text")
	if err != nil {
		t.Fatalf("verifyAgentAnswer returned error: %v", err)
	}
	if verification.Approved {
		t.Fatal("expected approval to be downgraded when the critic cites a failed observation as evidence")
	}
	if verification.Feedback == "" {
		t.Fatal("expected feedback explaining the rejected evidence")
	}
}

func TestIsTruncatedAgentJSON(t *testing.T) {
	_, truncatedErr := parseAgentAction(`{"action": "answer", "text": "partial`)
	if !isTruncatedAgentJSON(truncatedErr) {
		t.Fatalf("expected a cut-off JSON string to be classified as truncated, got: %v", truncatedErr)
	}

	_, schemaErr := parseAgentAction(`{"action": "not_a_real_action"}`)
	if isTruncatedAgentJSON(schemaErr) {
		t.Fatalf("expected an unsupported-action error to NOT be classified as truncated, got: %v", schemaErr)
	}

	_, trailingErr := parseAgentAction(`{"action":"answer","text":"hi"} {"action":"answer","text":"again"}`)
	if isTruncatedAgentJSON(trailingErr) {
		t.Fatalf("expected a trailing-content error to NOT be classified as truncated, got: %v", trailingErr)
	}
}

func TestAgentIterationBudgetPausesAndRenews(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess := &agentSession{ID: "budget", AgentMode: true, AttachedFilePath: "/retained/attachment"}
	sess.AgentRun = newAgentRun(sess.ID, "say hello", "", "")
	sess.AgentRun.Iteration = 42
	sess.AgentRun.SegmentIteration = sess.AgentRun.MaxIterations
	runAgent(context.Background(), sess)
	run := sess.agentSnapshot()
	if run.Status != agentWaitingContinue || !run.CompletedAt.IsZero() || sess.AttachedFilePath == "" {
		t.Fatalf("expected nonterminal pause retaining resources: %+v", run)
	}
	resumeAgent(sess, "")
	run = sess.agentSnapshot()
	if run.Status != agentRunning || run.Iteration != 42 || run.SegmentIteration != 0 || len(run.UserRequirements) != 0 {
		t.Fatalf("unexpected continued run: %+v", run)
	}
	for _, waiting := range []AgentRunStatus{agentWaitingInput, agentWaitingApproval} {
		sess.AgentRun.Status = waiting
		sess.AgentRun.SegmentIteration = 5
		resumeAgent(sess, "updated requirement")
		run = sess.agentSnapshot()
		if run.Iteration != 42 || run.SegmentIteration != 0 || run.Status != agentRunning {
			t.Fatalf("feedback did not renew budget: %+v", run)
		}
	}
}

func TestConfiguredAgentIterationLimit(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
	}{{"", 30}, {"0", 30}, {"-2", 30}, {"invalid", 30}, {"7", 7}} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv("RECON_MAX_ITERATIONS", test.value)
			if got := configuredAgentIterationLimit(); got != test.want {
				t.Fatalf("limit = %d, want %d", got, test.want)
			}
		})
	}
}

func TestAgentJobWaitDoesNotPollModel(t *testing.T) {
	for _, action := range []string{"check_job", "get_job"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			sess := &agentSession{ID: "wait-" + action}
			sess.AgentRun = newAgentRun(sess.ID, "collect result", "", "")
			job := jobStore.create(sess.ID, sess.AgentRun.ID, "test job")
			var calls atomic.Int32
			waiting := make(chan struct{})
			sess.eventSink = func(event AgentEvent) {
				if event.Kind == "status" && event.Action == action {
					close(waiting)
				}
			}
			ollama := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				content, _ := json.Marshal(ToolResponse{Action: action, JobID: job.ID})
				_ = json.NewEncoder(response).Encode(ChatResponse{Message: ChatMessage{Content: string(content)}})
			}))
			defer ollama.Close()
			previousURL := ollamaURL
			ollamaURL = ollama.URL
			t.Cleanup(func() { ollamaURL = previousURL })
			sess.AgentRun.MaxIterations = 1
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			finished := make(chan struct{})
			go func() {
				runAgent(ctx, sess)
				close(finished)
			}()
			select {
			case <-waiting:
			case <-ctx.Done():
				<-finished
				t.Fatal("worker did not wait for job")
			}
			select {
			case <-finished:
				t.Fatal("worker returned before job completion")
			case <-time.After(20 * time.Millisecond):
			}
			jobStore.finish(job.ID, "completed", "result", "")
			close(job.Done)
			<-finished
			run := sess.agentSnapshot()
			if calls.Load() != 1 || run.Iteration != 1 || run.Status != agentWaitingContinue {
				t.Fatalf("wait spent extra iterations or failed: calls=%d run=%+v", calls.Load(), run)
			}
			found := false
			for _, event := range run.Events {
				if event.Kind == "observation" && event.Action == action && event.Output == "result" && strings.Contains(event.Text, "completed") {
					found = true
				}
			}
			if !found {
				t.Fatal("missing final job observation")
			}
		})
	}
}

func TestAgentTerminalJobObservationRecordsCommandAndSuccess(t *testing.T) {
	for _, tc := range []struct {
		status  string
		success bool
	}{
		{"completed", true},
		{"failed", false},
		{"cancelled", false},
		{"timeout", false},
	} {
		for _, action := range []string{"check_job", "get_job"} {
			t.Run(tc.status+"/"+action, func(t *testing.T) {
				sess := &agentSession{ID: "terminal-" + tc.status + "-" + action}
				sess.AgentRun = newAgentRun(sess.ID, "collect result", "", "")
				command := "ping -c 1 127.0.0.1"
				job := jobStore.create(sess.ID, sess.AgentRun.ID, command)
				jobStore.finish(job.ID, tc.status, "result", "")

				if !executeAgentAction(context.Background(), sess, ToolResponse{Action: action, JobID: job.ID}) {
					t.Fatal("terminal job observation stopped agent execution")
				}
				events := sess.agentSnapshot().Events
				if len(events) != 1 {
					t.Fatalf("expected one observation, got %#v", events)
				}
				event := events[0]
				if event.Command != command || event.Success != tc.success || event.Action != action {
					t.Fatalf("unexpected terminal job observation: %#v", event)
				}
			})
		}
	}
}

func TestAgentJobWaitCancellationAndDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		sess := &agentSession{ID: fmt.Sprintf("wait-cancel-%t", deadline)}
		sess.AgentRun = newAgentRun(sess.ID, "collect result", "", "")
		job := jobStore.create(sess.ID, sess.AgentRun.ID, "test job")
		ctx, cancel := context.WithCancel(context.Background())
		want := agentCancelled
		if deadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			want = agentLimitReached
		} else {
			cancel()
		}
		if executeAgentAction(ctx, sess, ToolResponse{Action: "check_job", JobID: job.ID}) {
			t.Fatal("cancelled wait continued execution")
		}
		cancel()
		if run := sess.agentSnapshot(); run.Status != want || run.CompletedAt.IsZero() {
			t.Fatalf("status = %s, want %s", run.Status, want)
		}
		close(job.Done)
	}
}

func TestRunAgentTreatsTruncatedJSONAsRevisionNotFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var workerAttempts int32
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var chatRequest ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&chatRequest); err != nil {
			t.Errorf("decode ollama request: %v", err)
		}
		if len(chatRequest.Messages) > 0 && strings.Contains(chatRequest.Messages[0].Content, "independent critic") {
			_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: `{"approved":true,"feedback":"","evidenceEventIds":[]}`}})
			return
		}
		if atomic.AddInt32(&workerAttempts, 1) <= 4 {
			// Simulate Ollama returning a valid envelope whose message content is
			// itself cut off mid-JSON, as a long code answer would be.
			_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: `{"action": "answer", "text": "partial`}})
			return
		}
		_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: `{"action":"answer","text":"a short greeting"}`}})
	}))
	defer ollama.Close()
	previousURL := ollamaURL
	ollamaURL = ollama.URL
	t.Cleanup(func() { ollamaURL = previousURL })

	sess := &agentSession{ID: "sess-truncation"}
	sess.AgentRun = newAgentRun(sess.ID, "say hello", "", "")

	runAgent(context.Background(), sess)

	run := sess.agentSnapshot()
	if run.Status != agentWaitingApproval {
		t.Fatalf("expected the run to reach waiting_approval despite repeated truncation, got status=%q error=%q", run.Status, run.Error)
	}
	if run.ConsecutiveErrors != 0 {
		t.Fatalf("expected ConsecutiveErrors to stay at 0 (truncation must not spend the 3-strike budget), got %d", run.ConsecutiveErrors)
	}
	if got := atomic.LoadInt32(&workerAttempts); got < 5 {
		t.Fatalf("expected more than 3 worker attempts since truncation alone must not stop the run, got %d", got)
	}
}

func TestRunAgentGivesPhysicalNewlineOneStrikeFreeCorrection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var workerAttempts int32
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var chatRequest ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&chatRequest); err != nil {
			t.Errorf("decode ollama request: %v", err)
		}
		if len(chatRequest.Messages) > 0 && strings.Contains(chatRequest.Messages[0].Content, "independent critic") {
			_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: `{"approved":true,"feedback":"","evidenceEventIds":[]}`}})
			return
		}
		if atomic.AddInt32(&workerAttempts, 1) <= 2 {
			_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: `{"action":"run_command","command":"echo 'first\nsecond'"}`}})
			return
		}
		_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: `{"action":"answer","text":"done"}`}})
	}))
	defer ollama.Close()
	previousURL := ollamaURL
	ollamaURL = ollama.URL
	t.Cleanup(func() { ollamaURL = previousURL })

	sess := &agentSession{ID: "sess-physical-newline"}
	sess.AgentRun = newAgentRun(sess.ID, "say done", "", "")

	runAgent(context.Background(), sess)

	run := sess.agentSnapshot()
	if run.Status != agentWaitingApproval {
		t.Fatalf("expected waiting_approval after recovery, got status=%q error=%q", run.Status, run.Error)
	}
	revisions := 0
	failures := 0
	for _, event := range run.Events {
		if event.Kind == "verification" && event.Text == agentPhysicalNewlineRevision {
			revisions++
		}
		if event.Kind == "error" && strings.Contains(event.Text, errAgentCommandPhysicalNewline.Error()) {
			failures++
		}
	}
	if revisions != 1 {
		t.Fatalf("physical-newline revisions = %d, want exactly 1", revisions)
	}
	if failures != 1 {
		t.Fatalf("physical-newline failures = %d, want 1 after the repeated mistake", failures)
	}
}

func TestValidateAgentCommandCharacterizationOfExistingAcceptedForms(t *testing.T) {
	accepted := []string{
		`ls -la`,
		`cat file.txt | grep foo`,
		`echo a; echo b`,
		`ping -c 1 example.com &`,
		`grep "foo bar" file.txt`,
		`echo 'hello world'`,
	}
	for _, command := range accepted {
		t.Run(command, func(t *testing.T) {
			if err := validateAgentCommand(command); err != nil {
				t.Fatalf("expected %q to be accepted, got error: %v", command, err)
			}
		})
	}

	rejected := []string{
		`rm -rf /`,
		`/bin/ls`,
	}
	for _, command := range rejected {
		t.Run(command, func(t *testing.T) {
			if err := validateAgentCommand(command); err == nil {
				t.Fatalf("expected %q to be rejected", command)
			}
		})
	}
}

func TestValidateAgentCommandDistinguishesShellEscapeFromPhysicalNewline(t *testing.T) {
	if err := validateAgentCommand(`echo "first\nsecond"`); err != nil {
		t.Fatalf("expected a literal backslash-n shell escape to be accepted, got: %v", err)
	}

	if err := validateAgentCommand("echo \"first\nsecond\""); err == nil {
		t.Fatal("expected a physical newline in the decoded command to be rejected")
	}
}

func TestValidateAgentCommandShAndXargsReValidateTheirTarget(t *testing.T) {
	cases := []struct {
		name    string
		command string
		wantErr bool
	}{
		{"sh -c with allowlisted script", `sh -c "cat /etc/passwd"`, false},
		{"sh -c with disallowed script", `sh -c "rm -rf /"`, true},
		{"bare sh with no -c", `sh`, true},
		{"sh -s is not the safe form", `sh -s`, true},
		{"xargs with allowlisted target", `xargs cat`, false},
		{"xargs with disallowed target", `xargs rm`, true},
		{"pipeline into xargs allowlisted target", `find . -name "*.txt" | xargs cat`, false},
		{"pipeline into xargs disallowed target", `find . -name "*.txt" | xargs rm`, true},
		{"xargs value flag then allowlisted target", `find . | xargs -n 1 cat`, false},
		{"xargs value flag then disallowed target", `find . | xargs -n 1 rm`, true},
		{"xargs attached flag then allowlisted target", `find . | xargs -I{} cat {}`, false},
		{"nested xargs invoking sh -c disallowed", `xargs sh -c "rm -rf /"`, true},
		{"nested sh -c invoking xargs disallowed", `sh -c "xargs rm"`, true},
		{"second sh -c on the same line is disallowed", `sh -c "ls" && sh -c "rm -rf /"`, true},
		{"second sh -c on the same line is allowed", `sh -c "ls" && sh -c "cat /etc/passwd"`, false},
		{"unterminated quote is rejected", `echo "unterminated`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentCommand(tc.command)
			if tc.wantErr && err == nil {
				t.Fatalf("expected %q to be rejected", tc.command)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected %q to be accepted, got error: %v", tc.command, err)
			}
		})
	}
}

func withCapturedLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &buf
}

func withCriticApprovingOllamaServer(t *testing.T, workerHandler func(attempt int32) []byte) {
	t.Helper()
	var attempts int32
	withTestOllamaServer(t, func(w http.ResponseWriter, r *http.Request) {
		var chatRequest ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&chatRequest); err != nil {
			t.Errorf("decode ollama request: %v", err)
		}
		if len(chatRequest.Messages) > 0 && strings.Contains(chatRequest.Messages[0].Content, "independent critic") {
			_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: `{"approved":true,"feedback":"","evidenceEventIds":[]}`}})
			return
		}
		content := workerHandler(atomic.AddInt32(&attempts, 1))
		_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: string(content)}})
	})
}

func TestLogAgentHeuristicFormat(t *testing.T) {
	buf := withCapturedLog(t)

	logAgentHeuristic("run-123", "someHeuristic", "fired for a reason")

	if got := buf.String(); !strings.Contains(got, "[agent-heuristic][run-123] someHeuristic: fired for a reason") {
		t.Fatalf("unexpected log output: %q", got)
	}
}

func TestRunAgentLogsWhenCodeDeliverableHeuristicFires(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	buf := withCapturedLog(t)
	withCriticApprovingOllamaServer(t, func(attempt int32) []byte {
		if attempt == 1 {
			content, _ := json.Marshal(ToolResponse{Action: "answer", Text: "here is a description with no code"})
			return content
		}
		content, _ := json.Marshal(ToolResponse{Action: "answer", Text: "```go\npackage main\n```"})
		return content
	})

	sess := &agentSession{ID: "sess-code-heuristic"}
	sess.AgentRun = newAgentRun(sess.ID, "write code for a simple app", "", "")
	runAgent(context.Background(), sess)

	if !strings.Contains(buf.String(), "goalRequiresCodeDeliverable: blocked candidate answer with no fenced code block") {
		t.Fatalf("expected code-deliverable heuristic log line, got: %s", buf.String())
	}
}

func TestRunAgentLogsWhenRequiredCommandHeuristicBlocksAnswer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	buf := withCapturedLog(t)
	withCriticApprovingOllamaServer(t, func(int32) []byte {
		content, _ := json.Marshal(ToolResponse{Action: "answer", Text: "done without running echo"})
		return content
	})

	sess := &agentSession{ID: "sess-required-command"}
	sess.AgentRun = newAgentRun(sess.ID, "run echo to check connectivity", "", "")
	runAgent(context.Background(), sess)

	want := `requiredGoalCommand: blocked answer before a successful "echo" observation existed`
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("expected required-command heuristic log line, got: %s", buf.String())
	}
}

func TestRunAgentLogsWhenCriticApprovalIsDowngradedByRequiredCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	buf := withCapturedLog(t)
	withCriticApprovingOllamaServer(t, func(attempt int32) []byte {
		if attempt == 1 {
			content, _ := json.Marshal(ToolResponse{Action: "run_command", Command: "echo hi"})
			return content
		}
		content, _ := json.Marshal(ToolResponse{Action: "answer", Text: "connectivity confirmed"})
		return content
	})

	sess := &agentSession{ID: "sess-critic-downgrade"}
	sess.AgentRun = newAgentRun(sess.ID, "run echo to check connectivity", "", "")
	runAgent(context.Background(), sess)

	want := `requiredGoalCommand: critic approved but cited no successful "echo" observation; downgraded to revision`
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("expected required-command downgrade heuristic log line, got: %s", buf.String())
	}
}

func TestParseAgentActionAcceptsEachActionsOwnFields(t *testing.T) {
	cases := []string{
		`{"action":"answer","text":"done"}`,
		`{"action":"update_findings","text":"partial notes"}`,
		`{"action":"request_clarification","text":"which format?"}`,
		`{"action":"run_command","command":"ls -la"}`,
		`{"action":"run_command_bg","command":"ls -la"}`,
		`{"action":"check_job","jobId":"job_1"}`,
		`{"action":"get_job","jobId":"job_1"}`,
		`{"action":"cancel_job","jobId":"job_1"}`,
		`{"action":"list_jobs"}`,
		`{"action":"list_directory","path":"/tmp","maxDepth":2}`,
		`{"action":"search_files","path":"/tmp","pattern":"foo"}`,
		`{"action":"create_plan","steps":["step one"]}`,
		`{"action":"complete_step","stepId":1,"evidenceEventIds":[1,2]}`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseAgentAction(raw); err != nil {
				t.Fatalf("expected %s to be accepted, got error: %v", raw, err)
			}
		})
	}
}

func TestParseAgentActionRejectsCrossActionFields(t *testing.T) {
	cases := []string{
		`{"action":"answer","text":"done","command":"ls"}`,
		`{"action":"list_jobs","text":"ignored"}`,
		`{"action":"run_command","command":"ls","path":"/tmp"}`,
		`{"action":"check_job","jobId":"job_1","stepId":1}`,
		`{"action":"create_plan","steps":["a"],"jobId":"job_1"}`,
		`{"action":"complete_step","stepId":1,"command":"ls"}`,
		`{"action":"search_files","path":"/tmp","pattern":"foo","text":"extra"}`,
		`{"action":"list_directory","path":"/tmp","pattern":"foo"}`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseAgentAction(raw); err == nil {
				t.Fatalf("expected %s to be rejected for carrying a cross-action field", raw)
			}
		})
	}
}

func TestParseAgentActionFieldScopeViolationIsLogged(t *testing.T) {
	buf := withCapturedLog(t)

	if _, err := parseAgentAction(`{"action":"answer","text":"done","command":"ls"}`); err == nil {
		t.Fatal("expected the cross-action field to be rejected")
	}

	want := `[agent-schema] action "answer" carried out-of-scope field(s): command`
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("expected schema-scope violation log line, got: %s", buf.String())
	}
}

func TestPersistAccumulatesPersistenceStats(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess := &agentSession{ID: "sess-persist-stats"}
	sess.AgentRun = newAgentRun(sess.ID, "test goal", "", "")

	if err := sess.persist(); err != nil {
		t.Fatalf("first persist failed: %v", err)
	}
	if err := sess.persist(); err != nil {
		t.Fatalf("second persist failed: %v", err)
	}

	count, totalBytes, totalDuration := sess.persistenceStats()
	if count != 2 {
		t.Fatalf("expected 2 recorded persist() calls, got %d", count)
	}
	if totalBytes <= 0 {
		t.Fatalf("expected positive total bytes written, got %d", totalBytes)
	}
	if totalDuration < 0 {
		t.Fatalf("expected non-negative total duration, got %s", totalDuration)
	}
}

func TestFinishAgentLogsPersistenceStats(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	buf := withCapturedLog(t)

	sess := &agentSession{ID: "sess-finish-persist"}
	sess.AgentRun = newAgentRun(sess.ID, "test goal", "", "")
	if err := sess.persist(); err != nil {
		t.Fatalf("persist failed: %v", err)
	}

	finishAgent(sess, agentCompleted, "done", "")

	run := sess.agentSnapshot()
	want := "[agent-persist][" + run.ID + "] calls="
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("expected persistence stats log line, got: %s", buf.String())
	}
}

func TestPersistedSnapshotDoesNotExposePersistenceCounters(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess := &agentSession{ID: "sess-schema-check"}
	sess.AgentRun = newAgentRun(sess.ID, "test goal", "", "")
	if err := sess.persist(); err != nil {
		t.Fatalf("persist failed: %v", err)
	}

	path, err := reconAgentRunPath(sess.ID)
	if err != nil {
		t.Fatalf("reconAgentRunPath: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted snapshot: %v", err)
	}
	for _, forbidden := range []string{"persistCount", "persistBytes", "persistDuration"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("persisted snapshot must not expose in-memory measurement field %q", forbidden)
		}
	}
}
