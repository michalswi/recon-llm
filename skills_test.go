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
	skills, _, err := discoverAgentSkills()
	if err != nil || len(skills) != 0 {
		t.Fatalf("absent directory should be optional: %v, %v", skills, err)
	}
	writeTestSkill(t, "report", "---\r\nname: report\r\ndescription: >-\r\n  Summarize findings:\r\n  use a table.\r\n---\r\nReport only observed facts.\r\n")
	writeTestSkill(t, "analysis", "---\nname: analysis\ndescription: \"Analyze: supplied text\"\n---\nSeparate facts from assumptions.")
	skills, issues, err := discoverAgentSkills()
	if err != nil || len(issues) != 0 {
		t.Fatal(err, issues)
	}
	if len(skills) != 2 || skills[0].Name != "analysis" || skills[1].Name != "report" {
		t.Fatalf("expected sorted catalog: %#v", skills)
	}
	if skills[1].Description != "Summarize findings: use a table." || skills[1].Instructions != "Report only observed facts." {
		t.Fatalf("YAML or CRLF parsing failed: %#v", skills[1])
	}
}

func TestDiscoverAgentSkillsSkipsInvalidDocuments(t *testing.T) {
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
			writeTestSkill(t, "good", "---\nname: good\ndescription: Good skill\n---\nUse facts.")
			skills, issues, err := discoverAgentSkills()
			if err != nil {
				t.Fatalf("invalid skill must not fail discovery: %v", err)
			}
			if len(skills) != 1 || skills[0].Name != "good" {
				t.Fatalf("valid sibling should survive: %#v", skills)
			}
			if len(issues) != 1 || issues[0].Name != "report" || !strings.Contains(issues[0].Err.Error(), path) {
				t.Fatalf("expected a folder-keyed, path-specific issue, got %#v", issues)
			}
		})
	}
}

func TestDiscoverAgentSkillsSkipsNonRegularFileAndRejectsCatalogOverflow(t *testing.T) {
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
		skills, issues, err := discoverAgentSkills()
		if err != nil || len(skills) != 0 || len(issues) != 1 || issues[0].Name != "report" {
			t.Fatalf("expected symlink skill to be skipped: %v %v %v", skills, issues, err)
		}
	})
	t.Run("catalog overflow", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		for i := 0; i <= maxAgentSkills; i++ {
			name := fmt.Sprintf("skill-%d", i)
			writeTestSkill(t, name, fmt.Sprintf("---\nname: %s\ndescription: Analyze facts\n---\nUse facts.", name))
		}
		if _, _, err := discoverAgentSkills(); err == nil {
			t.Fatal("expected oversized catalog to be rejected")
		}
	})
	t.Run("invalid skills count toward the limit", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		for i := 0; i <= maxAgentSkills; i++ {
			writeTestSkill(t, fmt.Sprintf("skill-%d", i), "not a skill")
		}
		if _, _, err := discoverAgentSkills(); err == nil {
			t.Fatal("expected invalid candidates to count toward the catalog limit")
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

func TestTerminalAttachedSkillsControlOnlyQuickRuns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	previous := terminalSkillNames
	previousAuto := terminalSkillsAuto
	terminalSkillsAuto = false
	terminalSkillNames = nil
	t.Cleanup(func() { terminalSkillNames, terminalSkillsAuto = previous, previousAuto })
	for _, name := range []string{"report", "analysis"} {
		writeTestSkill(t, name, fmt.Sprintf("---\nname: %s\ndescription: Help with facts\n---\nFollow the %s procedure.", name, name))
	}
	if !handleAgentCLICommand("/skill attach report") {
		t.Fatal("skill command was not handled")
	}
	quick := newQuickAgentRequest("summarize facts")
	sess, err := newTerminalAgentSession(quick)
	if err != nil {
		t.Fatal(err)
	}
	sess.eventSink = nil
	run := sess.agentSnapshot()
	if !run.ManualSkills || len(run.ActiveSkills) != 1 || run.ActiveSkills[0].Name != "report" || len(run.AvailableSkills) != 1 {
		t.Fatalf("expected attached-only selection: %#v", run)
	}
	messages, err := agentMessages(sess)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(messages[0].Content, "Automatic skill selection is disabled") {
		t.Fatal("worker was not told automatic selection is disabled")
	}
	executeAgentAction(context.Background(), sess, ToolResponse{Action: "load_skill", Skill: "analysis"})
	if event := sess.agentSnapshot().Events[0]; event.Success {
		t.Fatal("worker loaded an unattached skill in manual mode")
	}
	// Structured/API requests retain their full catalog even while skills are attached.
	structured, err := newTerminalAgentSession(AgentRequest{Goal: "summarize", Skills: []string{"report"}})
	if err != nil {
		t.Fatal(err)
	}
	if structured.AgentRun.ManualSkills || len(structured.AgentRun.AvailableSkills) != 2 {
		t.Fatal("terminal attachments changed structured/API behavior")
	}
	if err := structured.activateAgentSkill("analysis"); err != nil {
		t.Fatal(err)
	}
	if err := handleTerminalSkillCommand([]string{"attach", "analysis"}); err != nil {
		t.Fatal(err)
	}
	multiple, err := newTerminalAgentSession(newQuickAgentRequest("use both"))
	if err != nil {
		t.Fatal(err)
	}
	if len(multiple.AgentRun.ActiveSkills) != 2 {
		t.Fatal("all attached skills must be activated before restricting the catalog")
	}
	if err := handleTerminalSkillCommand([]string{"detach", "report"}); err != nil {
		t.Fatal(err)
	}
	next := newQuickAgentRequest("next goal")
	if !next.attachedSkillsOnly || len(next.Skills) != 1 || next.Skills[0] != "analysis" {
		t.Fatalf("partial detach changed mode: %#v", next)
	}
	if quick.Skills[0] != "report" {
		t.Fatal("detaching mutated an existing request")
	}
	if err := handleTerminalSkillCommand([]string{"detach", "analysis"}); err != nil {
		t.Fatal(err)
	}
	off, err := newTerminalAgentSession(newQuickAgentRequest("no skills"))
	if err != nil {
		t.Fatal(err)
	}
	if !off.AgentRun.SkillsDisabled || len(off.AgentRun.AvailableSkills) != 0 {
		t.Fatal("last detach must turn skills off")
	}
	if err := handleTerminalSkillCommand([]string{"auto"}); err != nil {
		t.Fatal(err)
	}
	automatic, err := newTerminalAgentSession(newQuickAgentRequest("new goal"))
	if err != nil {
		t.Fatal(err)
	}
	if automatic.AgentRun.ManualSkills || len(automatic.AgentRun.ActiveSkills) != 0 || len(automatic.AgentRun.AvailableSkills) != 2 {
		t.Fatal("auto command did not enable automatic selection")
	}
	if err := automatic.activateAgentSkill("analysis"); err != nil {
		t.Fatal(err)
	}
	sess.AgentRun.Status = agentWaitingContinue
	resumeAgent(sess, "")
	if !sess.AgentRun.ManualSkills || sess.AgentRun.ActiveSkills[0].Name != "report" {
		t.Fatal("terminal detach changed an existing run")
	}
	if err := sess.activateAgentSkill("analysis"); err == nil {
		t.Fatal("continuation widened manual selection")
	}
}

func TestTerminalSkillCommandsValidateBeforeUpdating(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	previous := terminalSkillNames
	previousAuto := terminalSkillsAuto
	terminalSkillsAuto = false
	terminalSkillNames = nil
	t.Cleanup(func() { terminalSkillNames, terminalSkillsAuto = previous, previousAuto })
	writeTestSkill(t, "report", "---\nname: report\ndescription: Report facts\n---\nUse facts.")
	for i := 0; i < 2; i++ {
		if err := handleTerminalSkillCommand([]string{"attach", "report"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{nil, {"attach"}, {"attach", "missing"}, {"detach", "missing"}, {"list", "extra"}, {"auto", "extra"}, {"off", "extra"}, {"other"}} {
		if err := handleTerminalSkillCommand(args); err == nil {
			t.Fatalf("expected error for %v", args)
		}
		if len(terminalSkillNames) != 1 || terminalSkillNames[0] != "report" {
			t.Fatal("invalid command changed attached skills")
		}
	}
	if !handleAgentCLICommand("/skills") || !handleAgentCLICommand("/skill list") {
		t.Fatal("list commands were not handled")
	}
	if err := handleTerminalSkillCommand([]string{"detach"}); err != nil {
		t.Fatal(err)
	}
	if len(terminalSkillNames) != 0 {
		t.Fatal("detach without a name must clear the selection")
	}
}

func TestTerminalSkillsOffByDefaultAndModeSwitches(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	previous, previousAuto := terminalSkillNames, terminalSkillsAuto
	terminalSkillNames, terminalSkillsAuto = nil, false
	t.Cleanup(func() { terminalSkillNames, terminalSkillsAuto = previous, previousAuto })
	path := writeTestSkill(t, "report", "invalid skill file")
	sess, err := newTerminalAgentSession(newQuickAgentRequest("hello"))
	if err != nil {
		t.Fatalf("off mode should not read invalid installed skills: %v", err)
	}
	if !sess.AgentRun.SkillsDisabled || len(sess.AgentRun.ActiveSkills) != 0 || len(sess.AgentRun.AvailableSkills) != 0 {
		t.Fatal("skills must be off by default")
	}
	if err := sess.activateAgentSkill("report"); err == nil {
		t.Fatal("disabled run must reject skill activation")
	}
	messages, err := agentMessages(sess)
	if err != nil || !strings.Contains(messages[0].Content, "Skills are disabled") {
		t.Fatalf("disabled mode was not communicated: %v", err)
	}
	if err := os.WriteFile(path, []byte("---\nname: report\ndescription: Report facts\n---\nUse facts."), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"/skill attach report", "/skill auto", "/skill attach report", "/skill off"} {
		if !handleAgentCLICommand(command) {
			t.Fatalf("command not handled: %s", command)
		}
		request := newQuickAgentRequest("next")
		switch command {
		case "/skill attach report":
			if request.skillsDisabled || !request.attachedSkillsOnly || terminalSkillsAuto {
				t.Fatal("attach must enable attached-only mode")
			}
		case "/skill auto":
			if request.skillsDisabled || request.attachedSkillsOnly || len(request.Skills) != 0 {
				t.Fatal("auto must clear attachments and enable automatic selection")
			}
		case "/skill off":
			if !request.skillsDisabled || len(request.Skills) != 0 {
				t.Fatal("off must clear attachments and disable skills")
			}
		}
	}
	resumeAgent(sess, "")
	if !sess.AgentRun.SkillsDisabled || sess.activateAgentSkill("report") == nil {
		t.Fatal("mode changes must not enable skills on an existing disabled run")
	}
}

func TestInvalidSkillsAreWarnedAndExplicitSelectionExplains(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	previous, previousAuto := terminalSkillNames, terminalSkillsAuto
	terminalSkillNames, terminalSkillsAuto = nil, false
	t.Cleanup(func() { terminalSkillNames, terminalSkillsAuto = previous, previousAuto })
	writeTestSkill(t, "good", "---\nname: good\ndescription: Good skill\n---\nUse facts.")
	writeTestSkill(t, "broken", "---\nname: other\ndescription: Mismatched name\n---\nUse facts.")

	sess, err := newTerminalAgentSession(AgentRequest{Goal: "summarize facts"})
	if err != nil {
		t.Fatalf("invalid skill must not block run creation: %v", err)
	}
	run := sess.agentSnapshot()
	if len(run.AvailableSkills) != 1 || run.AvailableSkills[0].Name != "good" {
		t.Fatalf("expected only the valid skill: %#v", run.AvailableSkills)
	}
	warnings := 0
	for _, event := range run.Events {
		if event.Kind == "warning" && strings.Contains(event.Text, "broken") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("expected one warning event for the invalid skill: %#v", run.Events)
	}

	_, err = newTerminalAgentSession(AgentRequest{Goal: "summarize facts", Skills: []string{"broken"}})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("explicitly selecting an invalid skill must explain why: %v", err)
	}

	if err := handleTerminalSkillCommand([]string{"attach", "good"}); err != nil {
		t.Fatal(err)
	}
	if err := handleTerminalSkillCommand([]string{"attach", "broken"}); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("attaching an invalid skill must fail: %v", err)
	}
	if len(terminalSkillNames) != 1 || terminalSkillNames[0] != "good" {
		t.Fatalf("failed attach changed the selection: %v", terminalSkillNames)
	}
}

func TestDisabledSkillsSkipDiscoveryAndWarnings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeTestSkill(t, "broken", "not a skill")
	sess, err := newTerminalAgentSession(AgentRequest{Goal: "summarize facts", skillsDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range sess.agentSnapshot().Events {
		if event.Kind == "warning" {
			t.Fatalf("disabled skills must not scan or warn: %#v", event)
		}
	}
}

func TestSkillContextWarningThreshold(t *testing.T) {
	previous := ollamaNumCtx
	t.Cleanup(func() { ollamaNumCtx = previous })
	sess := &agentSession{AgentRun: &AgentRun{ActiveSkills: []AgentSkill{{
		SkillMetadata: SkillMetadata{Name: "report", Description: "Report"},
		Instructions:  strings.Repeat("x", 400),
	}}}}
	tokens := estimateAgentTokens(agentSkillInstructions(sess.AgentRun))

	ollamaNumCtx = 0
	if sess.skillContextWarning() != "" {
		t.Fatal("unset OLLAMA_NUM_CTX must not warn")
	}
	ollamaNumCtx = tokens * 4 // exactly 25%
	if sess.skillContextWarning() == "" {
		t.Fatal("expected a warning at exactly the threshold")
	}
	ollamaNumCtx = tokens*4 + 1 // just under 25%
	if sess.skillContextWarning() != "" {
		t.Fatal("unexpected warning below the threshold")
	}

	ollamaNumCtx = tokens * 4
	loaded := &agentSession{AgentRun: newAgentRun("run", "goal", "base", "base"), skillCatalog: sess.AgentRun.ActiveSkills}
	loaded.eventSink = nil
	executeAgentAction(context.Background(), loaded, ToolResponse{Action: "load_skill", Skill: "report"})
	var warned bool
	for _, event := range loaded.agentSnapshot().Events {
		warned = warned || event.Kind == "warning"
	}
	if !warned {
		t.Fatal("load_skill should emit a warning event when the threshold is reached")
	}
}
