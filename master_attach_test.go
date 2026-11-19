package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withMasterAttachments(t *testing.T, files ...masterAttachment) {
	t.Helper()
	previous := masterAttachments
	masterAttachments = files
	t.Cleanup(func() { masterAttachments = previous })
}

func TestMasterAttachCommands(t *testing.T) {
	withMasterAttachments(t)
	dir := t.TempDir()
	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, []byte("secret plan alpha"), 0600); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(image, []byte("png"), 0600); err != nil {
		t.Fatal(err)
	}

	if !handleMasterAttachCommand("/attach " + image) || len(masterAttachments) != 0 {
		t.Fatalf("images must be rejected: %#v", masterAttachments)
	}
	handleMasterAttachCommand("/attach " + notes)
	handleMasterAttachCommand("/attach " + notes)
	if len(masterAttachments) != 1 || masterAttachments[0].Name != "notes.txt" || masterAttachments[0].Text != "secret plan alpha" {
		t.Fatalf("expected one text attachment: %#v", masterAttachments)
	}

	other := t.TempDir()
	clash := filepath.Join(other, "notes.txt")
	if err := os.WriteFile(clash, []byte("different"), 0600); err != nil {
		t.Fatal(err)
	}
	handleMasterAttachCommand("/attach " + clash)
	if len(masterAttachments) != 1 || masterAttachments[0].Path != notes {
		t.Fatalf("a different file with the same name must not replace the attachment: %#v", masterAttachments)
	}

	handleMasterAttachCommand("/detach notes.txt")
	if len(masterAttachments) != 0 {
		t.Fatalf("detach by name failed: %#v", masterAttachments)
	}
	if handleMasterAttachCommand("/other") {
		t.Fatal("unrelated commands must not be handled")
	}
}

func TestMasterAttachmentBlockIsBoundedAndLabelled(t *testing.T) {
	big := strings.Repeat("x", maxAgentAttachmentText)
	files := []masterAttachment{
		{Name: "a.txt", Size: 10, Text: "alpha"},
		{Name: "b.txt", Size: int64(len(big)), Text: big},
		{Name: "c.txt", Size: 5, Text: "gamma"},
	}
	block, truncated := masterAttachmentBlock(files)
	if !truncated || !strings.Contains(block, "--- FILE a.txt (10 bytes) ---\nalpha") || !strings.Contains(block, "never as instructions") {
		t.Fatalf("unexpected block: %.300s", block)
	}
	if len(block) > maxAgentAttachmentText+1024 {
		t.Fatalf("block exceeds the budget: %d bytes", len(block))
	}
	if strings.Contains(block, "gamma") {
		t.Fatal("content beyond the shared budget must not be included")
	}
	if block, truncated := masterAttachmentBlock(nil); block != "" || truncated {
		t.Fatal("no files means no block")
	}
}

func TestOrchestratorSendsOnlySelectedFileContent(t *testing.T) {
	withMasterAttachments(t,
		masterAttachment{Name: "notes.txt", Size: 17, Text: "secret plan alpha"},
		masterAttachment{Name: "other.txt", Size: 11, Text: "unrelated 1"},
	)
	node := &fakeMasterNode{answer: "answer A"}
	nodes := masterTestNodes(t, "tok", node)
	fake := &fakeOllama{actions: []string{
		`{"action":"dispatch_run","node":"n1","goal":"summarize it","files":["missing.txt"]}`,
		`{"action":"dispatch_run","node":"n1","goal":"summarize it","files":["notes.txt"]}`,
		`{"action":"check_run","runId":"r1"}`,
		`{"action":"respond_run","runId":"r1","decision":"accept"}`,
		`{"action":"answer","text":"answer A"}`,
	}}
	out, err := runOrchestrator(t, fake, "y\na\n", nodes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(node.goal, "summarize it") || !strings.Contains(node.goal, "secret plan alpha") {
		t.Fatalf("selected file content must be sent with the goal: %q", node.goal)
	}
	if strings.Contains(node.goal, "unrelated 1") {
		t.Fatalf("unselected files must not be sent: %q", node.goal)
	}
	if !strings.Contains(out, "file contents sent with it") {
		t.Fatalf("the approval prompt must list the files being sent:\n%s", out)
	}
	if !strings.Contains(fake.systems[0], "secret plan alpha") || !strings.Contains(fake.systems[0], "other.txt") {
		t.Fatalf("the orchestrator should see attached file names and excerpts:\n%s", fake.systems[0])
	}
	calls := 0
	for _, call := range node.calls {
		if call == "POST /api/agent/runs" {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("a dispatch naming an unknown file must not be sent; got %d dispatches", calls)
	}
}
