package main

import (
	"os"
	"path/filepath"
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
