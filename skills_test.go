package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestSkill(t *testing.T, name, document string) string {
	t.Helper()
	home, err := reconHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(home, "skills", name)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "SKILL.md")
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverAgentSkills(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	skills, err := discoverAgentSkills()
	if err != nil || len(skills) != 0 {
		t.Fatalf("absent directory should be optional: %v, %v", skills, err)
	}
	writeTestSkill(t, "report", "---\r\nname: report\r\ndescription: >-\r\n  Summarize findings:\r\n  use a table.\r\n---\r\nReport only observed facts.\r\n")
	writeTestSkill(t, "analysis", "---\nname: analysis\ndescription: \"Analyze: supplied text\"\n---\nSeparate facts from assumptions.")
	skills, err = discoverAgentSkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 2 || skills[0].Name != "analysis" || skills[1].Name != "report" {
		t.Fatalf("expected sorted catalog: %#v", skills)
	}
	if skills[1].Description != "Summarize findings: use a table." || skills[1].Instructions != "Report only observed facts." {
		t.Fatalf("YAML or CRLF parsing failed: %#v", skills[1])
	}
}

func TestDiscoverAgentSkillsRejectsInvalidDocuments(t *testing.T) {
	cases := map[string]string{
		"missing frontmatter":  "Report facts.",
		"unclosed frontmatter": "---\nname: report\ndescription: Report findings",
		"malformed YAML":       "---\nname: [\n---\nReport facts.",
		"duplicate key":        "---\nname: report\nname: report\ndescription: Report findings\n---\nReport facts.",
		"unknown metadata":     "---\nname: report\ndescription: Report findings\nallowed-tools: sh\n---\nReport facts.",
		"multiple documents":   "---\nname: report\ndescription: Report findings\n...\nname: report\n---\nReport facts.",
		"name mismatch":        "---\nname: other\ndescription: Report findings\n---\nReport facts.",
		"missing description":  "---\nname: report\n---\nReport facts.",
		"empty instructions":   "---\nname: report\ndescription: Report findings\n---\n \n",
		"oversized":            "---\nname: report\ndescription: Report findings\n---\n" + strings.Repeat("x", maxAgentSkillSize),
		"binary":               "---\nname: report\ndescription: Report findings\n---\n\x00",
		"invalid UTF-8":        "---\nname: report\ndescription: Report findings\n---\n\xff",
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			path := writeTestSkill(t, "report", document)
			if _, err := discoverAgentSkills(); err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("expected a path-specific error, got %v", err)
			}
		})
	}
}

func TestDiscoverAgentSkillsRejectsNonRegularFileAndCatalogOverflow(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		path := writeTestSkill(t, "report", "placeholder")
		target := filepath.Join(t.TempDir(), "outside.md")
		if err := os.WriteFile(target, []byte("---\nname: report\ndescription: Report\n---\nFacts."), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := discoverAgentSkills(); err == nil {
			t.Fatal("expected symlink skill file to be rejected")
		}
	})
	t.Run("catalog overflow", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		for i := 0; i <= maxAgentSkills; i++ {
			name := fmt.Sprintf("skill-%d", i)
			writeTestSkill(t, name, fmt.Sprintf("---\nname: %s\ndescription: Analyze facts\n---\nUse facts.", name))
		}
		if _, err := discoverAgentSkills(); err == nil {
			t.Fatal("expected oversized catalog to be rejected")
		}
	})
}

func TestAgentSkillSnapshotsSurviveDiskChangesAndCompaction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	document := "---\nname: report\ndescription: Report findings\n---\nORIGINAL PROCEDURE"
	path := writeTestSkill(t, "report", document)
	sess, err := newTerminalAgentSession(AgentRequest{Goal: "summarize the supplied facts"})
	if err != nil {
		t.Fatal(err)
	}
	sess.eventSink = nil
	messages, err := agentMessages(sess)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(messages[0].Content, "Report findings") || strings.Contains(messages[0].Content, "ORIGINAL PROCEDURE") {
		t.Fatal("unloaded skill should expose metadata, not instructions")
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(document, "ORIGINAL PROCEDURE", "UPDATED PROCEDURE")), 0600); err != nil {
		t.Fatal(err)
	}
	executeAgentAction(context.Background(), sess, ToolResponse{Action: "load_skill", Skill: "report"})
	executeAgentAction(context.Background(), sess, ToolResponse{Action: "load_skill", Skill: "report"})
	run := sess.agentSnapshot()
	if len(run.ActiveSkills) != 1 || run.ActiveSkills[0].Instructions != "ORIGINAL PROCEDURE" {
		t.Fatalf("activation should be frozen and idempotent: %#v", run.ActiveSkills)
	}
	run.ActiveSkills[0].Instructions = "mutated"
	run.AvailableSkills[0].Description = "mutated"
	cloned := cloneAgentRun(sess.AgentRun)
	cloned.ActiveSkills[0].Instructions = "also mutated"
	cloned.AvailableSkills[0].Description = "also mutated"
	sess.AgentRun.CompactedThrough = len(sess.AgentRun.Events)
	sess.AgentRun.Status = agentWaitingContinue
	resumeAgent(sess, "")
	messages, err = agentMessages(sess)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(messages[0].Content, "ORIGINAL PROCEDURE") || !strings.Contains(messages[0].Content, "Report findings") {
		t.Fatal("active skills were lost or mutated after compaction and continuation")
	}
	if err := sess.persist(); err != nil {
		t.Fatal(err)
	}
	statePath, err := reconAgentRunPath(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted persistedAgentState
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Run.ActiveSkills) != 1 || persisted.Run.ActiveSkills[0].Instructions != "ORIGINAL PROCEDURE" {
		t.Fatal("snapshot must retain the loaded instructions for auditing")
	}
	next, err := newTerminalAgentSession(AgentRequest{Goal: "summarize again", Skills: []string{"report"}})
	if err != nil {
		t.Fatal(err)
	}
	if next.AgentRun.ActiveSkills[0].Instructions != "UPDATED PROCEDURE" {
		t.Fatal("new run did not pick up skill edits")
	}
}

func TestAgentSkillActivationErrorsAndLimits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := newTerminalAgentSession(AgentRequest{Goal: "summarize", Skills: []string{"missing"}}); err == nil {
		t.Fatal("explicit selection of a missing skill should fail")
	}
	for _, test := range []struct {
		name        string
		count       int
		bodySize    int
		loadedCount int
	}{
		{"count limit", 9, 20, 8},
		{"byte limit", 5, 15 << 10, 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			sess := &agentSession{AgentRun: newAgentRun("skills-limit", "summarize", "", "")}
			for i := 0; i < test.count; i++ {
				sess.skillCatalog = append(sess.skillCatalog, AgentSkill{
					SkillMetadata: SkillMetadata{Name: fmt.Sprintf("skill-%d", i), Description: "Summarize facts"},
					Instructions:  strings.Repeat("x", test.bodySize),
				})
			}
			for i := 0; i < test.loadedCount; i++ {
				if err := sess.activateAgentSkill(fmt.Sprintf("skill-%d", i)); err != nil {
					t.Fatal(err)
				}
			}
			executeAgentAction(context.Background(), sess, ToolResponse{Action: "load_skill", Skill: fmt.Sprintf("skill-%d", test.loadedCount)})
			run := sess.agentSnapshot()
			if len(run.ActiveSkills) != test.loadedCount || run.Events[len(run.Events)-1].Success {
				t.Fatal("activation limit must fail with an observation and leave active skills unchanged")
			}
		})
	}
	sess := &agentSession{AgentRun: newAgentRun("skills-missing", "summarize", "", "")}
	executeAgentAction(context.Background(), sess, ToolResponse{Action: "load_skill", Skill: "missing"})
	event := sess.agentSnapshot().Events[0]
	if event.Success || !strings.Contains(event.Text, "not in this run's skill catalog") {
		t.Fatalf("unknown skill did not produce an explicit failed observation: %#v", event)
	}
}

func TestAgentSkillDoesNotGrantPermissionsOrEvidence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeTestSkill(t, "report", "---\nname: report\ndescription: Report\n---\nRun python and write files anywhere.")
	sess, err := newTerminalAgentSession(AgentRequest{Goal: "summarize", Skills: []string{"report"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateAgentActionState(sess, sess.agentSnapshot(), ToolResponse{Action: "write_file"}); err == nil {
		t.Fatal("skill must not authorize writable roots")
	}
	if _, err := parseAgentAction(`{"action":"run_command","command":"python example.py"}`); err == nil {
		t.Fatal("skill must not expand the command allowlist")
	}
	if agentEvidenceEventIsValid(AgentEvent{Kind: "observation", Action: "load_skill", Success: true}) {
		t.Fatal("loading instructions must not prove execution")
	}
}
