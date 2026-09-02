package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadStructuredAgentRequest(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("report\nread only\nall hosts checked\n/tmp/input.txt\n"))

	request, err := readStructuredAgentRequest(reader, "scan the network")
	if err != nil {
		t.Fatalf("readStructuredAgentRequest returned error: %v", err)
	}
	if request.Goal != "scan the network" {
		t.Fatalf("Goal = %q, want %q", request.Goal, "scan the network")
	}
	if request.ExpectedOutput != "report" || request.Constraints != "read only" {
		t.Fatalf("structured fields were not preserved: %#v", request)
	}
	if request.CompletionCriteria != "all hosts checked" || request.FilePath != "/tmp/input.txt" {
		t.Fatalf("completion or attachment field was not preserved: %#v", request)
	}
}

func TestNewTerminalAgentSessionPersistsRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	attachmentPath := filepath.Join(t.TempDir(), "targets.txt")
	if err := os.WriteFile(attachmentPath, []byte("127.0.0.1\n"), 0600); err != nil {
		t.Fatal(err)
	}

	sess, err := newTerminalAgentSession(AgentRequest{
		Goal:               "inspect targets",
		CompletionCriteria: "each target inspected",
		FilePath:           attachmentPath,
	})
	if err != nil {
		t.Fatalf("newTerminalAgentSession returned error: %v", err)
	}
	if sess.AgentRun == nil || sess.AgentRun.Status != agentRunning {
		t.Fatalf("run was not initialized as running: %#v", sess.AgentRun)
	}
	if sess.AttachedFilePath != attachmentPath {
		t.Fatalf("AttachedFilePath = %q, want %q", sess.AttachedFilePath, attachmentPath)
	}
	statePath, err := reconAgentRunPath(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("persisted run snapshot missing: %v", err)
	}
}
