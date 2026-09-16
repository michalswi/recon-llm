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

func TestComposeAgentPromptWithoutPromptDirectory(t *testing.T) {
	t.Chdir(t.TempDir())

	prompt, err := composeAgentPrompt("base", "test goal", "test history")
	if err != nil {
		t.Fatalf("composeAgentPrompt returned error: %v", err)
	}
	if !strings.Contains(prompt, "test goal") || !strings.Contains(prompt, "test history") {
		t.Fatalf("composed prompt does not contain substitutions: %q", prompt)
	}
}

func TestImagePathInGoalAttachesImageToAgentMessage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	imagePath := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(imagePath, []byte{1, 2, 3}, 0600); err != nil {
		t.Fatal(err)
	}

	sess, err := newTerminalAgentSession(AgentRequest{Goal: "describe this image " + imagePath})
	if err != nil {
		t.Fatalf("newTerminalAgentSession returned error: %v", err)
	}
	messages, err := agentMessages(sess)
	if err != nil {
		t.Fatalf("agentMessages returned error: %v", err)
	}
	images := messages[len(messages)-1].Images
	if len(images) != 1 || images[0] != "AQID" {
		t.Fatalf("message images = %#v, want base64 image payload", images)
	}
	if command := requiredGoalCommand(sess.AgentRun); command != "" {
		t.Fatalf("requiredGoalCommand = %q, want no command requirement", command)
	}
}

func TestRequiredGoalCommandRequiresExplicitExecution(t *testing.T) {
	run := newAgentRun("session", "", "", "")
	run.GoalSpec.Objective = "describe this file for a report and sort the findings"
	if command := requiredGoalCommand(run); command != "" {
		t.Fatalf("requiredGoalCommand = %q, want no command requirement", command)
	}
	run.GoalSpec.Objective = "please execute ping example.com"
	if command := requiredGoalCommand(run); command != "ping" {
		t.Fatalf("requiredGoalCommand = %q, want ping", command)
	}
}

func TestPersistentDirectoryAttachmentsSendAllImages(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	directory := t.TempDir()
	firstPath := filepath.Join(directory, "first.jpg")
	secondPath := filepath.Join(directory, "second.png")
	if err := os.WriteFile(firstPath, []byte{1}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte{2}, 0600); err != nil {
		t.Fatal(err)
	}

	terminalAttachmentPaths = nil
	t.Cleanup(func() { terminalAttachmentPaths = nil })
	if !handleAgentCLICommand("/attach " + directory) {
		t.Fatal("/attach was not handled")
	}
	sess, err := newTerminalAgentSession(AgentRequest{Goal: "describe the attached images", FilePaths: terminalAttachmentPaths})
	if err != nil {
		t.Fatalf("newTerminalAgentSession returned error: %v", err)
	}
	messages, err := agentMessages(sess)
	if err != nil {
		t.Fatalf("agentMessages returned error: %v", err)
	}
	images := messages[len(messages)-1].Images
	if len(images) != 2 || images[0] != "AQ==" || images[1] != "Ag==" {
		t.Fatalf("message images = %#v, want both directory images", images)
	}
	if !handleAgentCLICommand("/deattach") || len(terminalAttachmentPaths) != 0 {
		t.Fatal("/deattach did not clear persistent attachments")
	}
}

func TestDeattachWithPathRemovesOnlyThatFile(t *testing.T) {
	firstPath := filepath.Join(t.TempDir(), "a.txt")
	secondPath := filepath.Join(t.TempDir(), "b.txt")
	if err := os.WriteFile(firstPath, []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte("b"), 0600); err != nil {
		t.Fatal(err)
	}

	terminalAttachmentPaths = nil
	t.Cleanup(func() { terminalAttachmentPaths = nil })
	if !handleAgentCLICommand("/attach " + firstPath) {
		t.Fatal("/attach was not handled")
	}
	if !handleAgentCLICommand("/attach " + secondPath) {
		t.Fatal("/attach was not handled")
	}
	if !handleAgentCLICommand("/deattach " + firstPath) {
		t.Fatal("/deattach was not handled")
	}
	if len(terminalAttachmentPaths) != 1 || terminalAttachmentPaths[0] != secondPath {
		t.Fatalf("terminalAttachmentPaths = %#v, want only %q", terminalAttachmentPaths, secondPath)
	}
}

func TestNonImageAttachmentNoteReachesModelPrompt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	docPath := filepath.Join(t.TempDir(), "rules.pdf")
	if err := os.WriteFile(docPath, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}

	sess, err := newTerminalAgentSession(AgentRequest{Goal: "make description of this file", FilePath: docPath})
	if err != nil {
		t.Fatalf("newTerminalAgentSession returned error: %v", err)
	}
	if sess.AgentRun.AttachmentNote == "" {
		t.Fatal("AttachmentNote was not set on the run")
	}
	messages, err := agentMessages(sess)
	if err != nil {
		t.Fatalf("agentMessages returned error: %v", err)
	}
	if !strings.Contains(messages[0].Content, "rules.pdf") {
		t.Fatalf("system prompt does not mention attached file: %q", messages[0].Content)
	}
}

func TestDirectoryAttachmentsUseShortExcerpts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	directory := t.TempDir()
	content := strings.Repeat("x", maxAgentDirectoryExcerpt+1)
	if err := os.WriteFile(filepath.Join(directory, "large.txt"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	sess, err := newTerminalAgentSession(AgentRequest{Goal: "describe files", FilePath: directory})
	if err != nil {
		t.Fatalf("newTerminalAgentSession returned error: %v", err)
	}
	if strings.Contains(sess.AgentRun.AttachmentNote, content) {
		t.Fatal("directory attachment included the complete file instead of an excerpt")
	}
	if !strings.Contains(sess.AgentRun.AttachmentNote, strings.Repeat("x", maxAgentDirectoryExcerpt)) {
		t.Fatal("directory attachment did not include the expected excerpt")
	}
	if !strings.Contains(sess.AgentRun.AttachmentNote, "Excerpt only") {
		t.Fatal("directory attachment did not tell the agent how to inspect more content")
	}
}

func TestAttachmentTextHasAggregateBudget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	directory := t.TempDir()
	paths := make([]string, 0, 3)
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(strings.Repeat("~", agentMaxObservation)), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}

	sess, err := newTerminalAgentSession(AgentRequest{Goal: "describe files", FilePaths: paths})
	if err != nil {
		t.Fatalf("newTerminalAgentSession returned error: %v", err)
	}
	if got := strings.Count(sess.AgentRun.AttachmentNote, "~"); got != maxAgentAttachmentText {
		t.Fatalf("inlined attachment text = %d bytes, want aggregate cap %d", got, maxAgentAttachmentText)
	}
}
