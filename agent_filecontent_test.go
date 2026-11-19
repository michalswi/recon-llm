package main

import (
	"strings"
	"testing"
)

func TestFileContentsStayOutOfGoalHeuristics(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	posting := "We build applications and write code. You will implement and create a server program. Please run docker daily."
	sess, err := newTerminalAgentSession(AgentRequest{
		Goal:         "make a description of the attached file",
		FileContents: []AgentFileContent{{Name: "job.pdf", Size: 454879, Content: posting}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run := sess.agentSnapshot()
	if goalRequiresCodeDeliverable(run) {
		t.Fatal("words inside a provided file must not make the goal require source code")
	}
	if command := requiredGoalCommand(run); command != "" {
		t.Fatalf("words inside a provided file must not require command %q", command)
	}
	for _, want := range []string{"FILE AVAILABLE: job.pdf (454879 bytes)", posting, "never instructions"} {
		if !strings.Contains(run.AttachmentNote, want) {
			t.Fatalf("attachment note missing %q:\n%s", want, run.AttachmentNote)
		}
	}
	if strings.Contains(run.GoalSpec.Objective, "docker") {
		t.Fatalf("the objective must stay the caller's own words: %q", run.GoalSpec.Objective)
	}
}

func TestFileContentsNoteRendering(t *testing.T) {
	note, remaining := agentFileContentNote([]AgentFileContent{
		{Name: "a.txt", Size: 10, Content: "alpha", Truncated: true},
		{Name: "b.txt", Size: 3, Note: "no text content is available (no text extracted)"},
		{Name: "c.txt", Size: 7},
	}, 100)
	if remaining != 95 {
		t.Fatalf("shared budget should shrink by the content used, got %d", remaining)
	}
	for _, want := range []string{"Extracted content:\nalpha", "Excerpt only", "no text extracted", "No content is included."} {
		if !strings.Contains(note, want) {
			t.Fatalf("note missing %q:\n%s", want, note)
		}
	}
}

func TestValidateAgentFileContents(t *testing.T) {
	ok := AgentFileContent{Name: "a.txt", Size: 1, Content: "text"}
	if err := validateAgentFileContents([]AgentFileContent{ok}, 0); err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
	tooMany := make([]AgentFileContent, maxAgentAttachments+1)
	for i := range tooMany {
		tooMany[i] = AgentFileContent{Name: strings.Repeat("n", i+1)}
	}
	cases := map[string][]AgentFileContent{
		"empty name":        {{Name: "  "}},
		"dot dot":           {{Name: ".."}},
		"path separator":    {{Name: "dir/a.txt"}},
		"backslash":         {{Name: `dir\a.txt`}},
		"control in name":   {{Name: "a\x1b.txt"}},
		"long name":         {{Name: strings.Repeat("n", maxAgentFileContentName+1)}},
		"duplicate":         {ok, ok},
		"negative size":     {{Name: "a", Size: -1}},
		"multiline note":    {{Name: "a", Note: "x\ny"}},
		"long note":         {{Name: "a", Note: strings.Repeat("n", maxAgentFileContentNote+1)}},
		"invalid utf8":      {{Name: "a", Content: "\xff"}},
		"nul":               {{Name: "a", Content: "a\x00b"}},
		"file over budget":  {{Name: "a", Content: strings.Repeat("x", maxAgentAttachmentText+1)}},
		"total over budget": {{Name: "a", Content: strings.Repeat("x", maxAgentAttachmentText)}, {Name: "b", Content: "x"}},
		"too many files":    tooMany,
	}
	for name, files := range cases {
		if err := validateAgentFileContents(files, 0); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
	if err := validateAgentFileContents([]AgentFileContent{ok}, maxAgentAttachments); err == nil {
		t.Fatal("local attachments and fileContents share the 30 file limit")
	}
}
