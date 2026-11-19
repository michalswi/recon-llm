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

	if !handleMasterAttachCommand("/attach "+image) || len(masterAttachments) != 0 {
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

func TestMasterFileContentsFollowReconLimits(t *testing.T) {
	big := strings.Repeat("Q", maxAgentAttachmentText)
	files := []masterAttachment{
		{Name: "dir.txt", Size: 5000, Text: strings.Repeat("Z", 5000), FromDir: true},
		{Name: "single.txt", Size: int64(len(big)), Text: big},
		{Name: "late.txt", Size: 5, Text: "gamma"},
		{Name: "empty.txt", Size: 0, Note: "no text content is available (no text extracted)"},
	}
	contents, truncated := masterFileContents(files)
	if !truncated || len(contents) != 4 {
		t.Fatalf("expected four entries with truncation: %#v", contents)
	}
	if len(contents[0].Content) != maxAgentDirectoryExcerpt || !contents[0].Truncated {
		t.Fatalf("a directory file must carry exactly a %d byte excerpt: %d", maxAgentDirectoryExcerpt, len(contents[0].Content))
	}
	total := 0
	for _, item := range contents {
		total += len(item.Content)
	}
	if total > maxAgentAttachmentText {
		t.Fatalf("contents exceed the shared budget: %d bytes", total)
	}
	if contents[2].Content != "" || !contents[2].Truncated || contents[2].Name != "late.txt" {
		t.Fatalf("a file past the shared budget must be listed without content: %#v", contents[2])
	}
	if contents[3].Note == "" || contents[3].Truncated {
		t.Fatalf("a file with no text must stay listed with its reason: %#v", contents[3])
	}
	if err := validateAgentFileContents(contents, 0); err != nil {
		t.Fatalf("what the master sends must pass the node's validation: %v", err)
	}

	single, truncated := masterFileContents([]masterAttachment{{Name: "one.txt", Size: int64(len(big)), Text: big}})
	if truncated || len(single[0].Content) != maxAgentAttachmentText || single[0].Truncated {
		t.Fatalf("a single file may use the whole 64 KiB budget untruncated (truncated=%v)", truncated)
	}
	if empty, truncated := masterFileContents(nil); len(empty) != 0 || truncated {
		t.Fatal("no files means no contents")
	}
}

func TestMasterAttachDirectoryAndUnreadableFiles(t *testing.T) {
	withMasterAttachments(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(strings.Repeat("a", 3000)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	handleMasterAttachCommand("/attach " + dir)
	if len(masterAttachments) != 2 {
		t.Fatalf("both directory files should be attached: %#v", masterAttachments)
	}
	byName := map[string]masterAttachment{}
	for _, file := range masterAttachments {
		byName[file.Name] = file
	}
	if !byName["a.txt"].FromDir || byName["a.txt"].sentBytes() != maxAgentDirectoryExcerpt || !byName["a.txt"].partial() {
		t.Fatalf("directory files get a 1 KiB excerpt: %#v", byName["a.txt"])
	}
	if byName["empty.txt"].Note == "" {
		t.Fatalf("a file without text must be kept with a note: %#v", byName["empty.txt"])
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
	if node.goal != "summarize it" {
		t.Fatalf("file text must not be mixed into the goal: %q", node.goal)
	}
	if len(node.files) != 1 || node.files[0].Name != "notes.txt" || node.files[0].Content != "secret plan alpha" {
		t.Fatalf("only the selected file content must be sent in fileContents: %#v", node.files)
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

func TestOrchestratorDispatchSendsAllAttachedFilesByDefault(t *testing.T) {
	withMasterAttachments(t,
		masterAttachment{Name: "a.txt", Size: 5, Text: "alpha"},
		masterAttachment{Name: "b.txt", Size: 4, Text: "beta"},
	)
	run := func(files string) *fakeMasterNode {
		node := &fakeMasterNode{answer: "answer A"}
		nodes := masterTestNodes(t, "tok", node)
		fake := &fakeOllama{actions: []string{
			`{"action":"dispatch_run","node":"n1","goal":"describe it"` + files + `}`,
			`{"action":"answer","text":"done"}`,
		}}
		if _, err := runOrchestrator(t, fake, "y\ns\n", nodes); err != nil {
			t.Fatal(err)
		}
		return node
	}

	if node := run(``); len(node.files) != 2 || node.files[0].Content != "alpha" || node.files[1].Content != "beta" {
		t.Fatalf("omitting files must send every attached file: %#v", node.files)
	}
	if node := run(`,"files":["b.txt"]`); len(node.files) != 1 || node.files[0].Name != "b.txt" {
		t.Fatalf("an explicit list sends only those files: %#v", node.files)
	}
	if node := run(`,"files":[]`); len(node.files) != 0 || node.goal != "describe it" {
		t.Fatalf("an explicit empty list opts out: %#v", node.files)
	}
}
