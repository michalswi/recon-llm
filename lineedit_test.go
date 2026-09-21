package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func newTestHistory(entries ...string) *promptHistory {
	return &promptHistory{entries: append([]string(nil), entries...)}
}

func TestRunLineEditorInsertsCharactersAndSubmitsOnEnter(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("hello\r"))
	line, err := runLineEditor(reader, "> ", newTestHistory())
	if err != nil {
		t.Fatalf("runLineEditor returned error: %v", err)
	}
	if line != "hello" {
		t.Fatalf("line = %q, want %q", line, "hello")
	}
}

func TestRunLineEditorBackspaceRemovesPrecedingCharacter(t *testing.T) {
	// Type "helxo", backspace twice (removing "xo"), then type "lo" -> "hello".
	reader := bufio.NewReader(strings.NewReader("helxo\x7f\x7flo\r"))
	line, err := runLineEditor(reader, "> ", newTestHistory())
	if err != nil {
		t.Fatalf("runLineEditor returned error: %v", err)
	}
	if line != "hello" {
		t.Fatalf("line = %q, want %q", line, "hello")
	}
}

func TestRunLineEditorLeftArrowMovesCursorForInsertion(t *testing.T) {
	// Type "helo", move left twice (past 'o' and 'l'), insert "l" -> "hello".
	reader := bufio.NewReader(strings.NewReader("helo\x1b[D\x1b[Dl\r"))
	line, err := runLineEditor(reader, "> ", newTestHistory())
	if err != nil {
		t.Fatalf("runLineEditor returned error: %v", err)
	}
	if line != "hello" {
		t.Fatalf("line = %q, want %q", line, "hello")
	}
}

func TestRunLineEditorRightArrowMovesCursorForward(t *testing.T) {
	// Type "abcd", move to start with 4 lefts, move right twice, insert "X" -> "abXcd".
	reader := bufio.NewReader(strings.NewReader("abcd\x1b[D\x1b[D\x1b[D\x1b[D\x1b[C\x1b[CX\r"))
	line, err := runLineEditor(reader, "> ", newTestHistory())
	if err != nil {
		t.Fatalf("runLineEditor returned error: %v", err)
	}
	if line != "abXcd" {
		t.Fatalf("line = %q, want %q", line, "abXcd")
	}
}

func TestRunLineEditorUpArrowRecallsHistory(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("\x1b[A\r"))
	line, err := runLineEditor(reader, "> ", newTestHistory("first objective", "second objective"))
	if err != nil {
		t.Fatalf("runLineEditor returned error: %v", err)
	}
	if line != "second objective" {
		t.Fatalf("line = %q, want most recent history entry", line)
	}
}

func TestRunLineEditorUpUpDownRestoresOlderThenNewerEntry(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("\x1b[A\x1b[A\x1b[B\r"))
	line, err := runLineEditor(reader, "> ", newTestHistory("first objective", "second objective"))
	if err != nil {
		t.Fatalf("runLineEditor returned error: %v", err)
	}
	if line != "second objective" {
		t.Fatalf("line = %q, want %q after up,up,down", line, "second objective")
	}
}

func TestRunLineEditorDownArrowPastNewestRestoresDraft(t *testing.T) {
	// Type a draft, recall history with Up, then Down past the newest entry
	// should restore the original in-progress draft.
	reader := bufio.NewReader(strings.NewReader("draft\x1b[A\x1b[B\r"))
	line, err := runLineEditor(reader, "> ", newTestHistory("first objective"))
	if err != nil {
		t.Fatalf("runLineEditor returned error: %v", err)
	}
	if line != "draft" {
		t.Fatalf("line = %q, want restored draft %q", line, "draft")
	}
}

func TestRunLineEditorSubmittedLineIsAddedToHistory(t *testing.T) {
	history := newTestHistory("existing")
	reader := bufio.NewReader(strings.NewReader("new objective\r"))
	if _, err := runLineEditor(reader, "> ", history); err != nil {
		t.Fatalf("runLineEditor returned error: %v", err)
	}
	if len(history.entries) != 2 || history.entries[1] != "new objective" {
		t.Fatalf("history.entries = %#v, want existing entry plus new objective", history.entries)
	}
}

func TestRunLineEditorLongPasteDoesNotRedrawForEveryByte(t *testing.T) {
	const label = "[goal]: "
	input := strings.Repeat("long objective ", 30)
	reader := bufio.NewReader(strings.NewReader(input + "\r"))
	var output bytes.Buffer

	line, err := runLineEditorWithOutput(reader, label, newTestHistory(), &output)
	if err != nil {
		t.Fatalf("runLineEditorWithOutput returned error: %v", err)
	}
	if line != strings.TrimSpace(input) {
		t.Fatalf("line length = %d, want %d", len(line), len(strings.TrimSpace(input)))
	}
	if count := strings.Count(output.String(), label); count != 1 {
		t.Fatalf("prompt label rendered %d times, want once; output = %q", count, output.String())
	}
}

func TestRunLineEditorLongHistoryRecallClearsFromPromptOrigin(t *testing.T) {
	const label = "[goal]: "
	historyEntry := strings.Repeat("long recalled objective ", 20)
	reader := bufio.NewReader(strings.NewReader("\x1b[A\r"))
	var output bytes.Buffer

	line, err := runLineEditorWithOutput(reader, label, newTestHistory(historyEntry), &output)
	if err != nil {
		t.Fatalf("runLineEditorWithOutput returned error: %v", err)
	}
	if line != strings.TrimSpace(historyEntry) {
		t.Fatalf("line length = %d, want %d", len(line), len(strings.TrimSpace(historyEntry)))
	}
	if redraws := strings.Count(output.String(), label); redraws != 2 {
		t.Fatalf("prompt render count = %d, want initial draw plus history recall", redraws)
	}
}

func TestRunLineEditorArrowMovementDoesNotRedrawLongHistory(t *testing.T) {
	const label = "[goal]: "
	historyEntry := strings.Repeat("long recalled objective ", 20)
	reader := bufio.NewReader(strings.NewReader("\x1b[A\x1b[D\x1b[D\x1b[C\x1b[D\r"))
	var output bytes.Buffer

	line, err := runLineEditorWithOutputAndWidth(reader, label, newTestHistory(historyEntry), &output, 40)
	if err != nil {
		t.Fatalf("runLineEditorWithOutputAndWidth returned error: %v", err)
	}
	if line != strings.TrimSpace(historyEntry) {
		t.Fatalf("line length = %d, want %d", len(line), len(strings.TrimSpace(historyEntry)))
	}
	if redraws := strings.Count(output.String(), label); redraws != 2 {
		t.Fatalf("prompt render count after repeated arrows = %d, want 2", redraws)
	}
}

func TestRunLineEditorArrowMovementDoesNotRedrawLongPaste(t *testing.T) {
	const label = "[goal]: "
	input := strings.Repeat("long pasted objective ", 20)
	reader := bufio.NewReader(strings.NewReader(input + "\x1b[D\x1b[D\x1b[C\r"))
	var output bytes.Buffer

	line, err := runLineEditorWithOutputAndWidth(reader, label, newTestHistory(), &output, 40)
	if err != nil {
		t.Fatalf("runLineEditorWithOutputAndWidth returned error: %v", err)
	}
	if line != strings.TrimSpace(input) {
		t.Fatalf("line length = %d, want %d", len(line), len(strings.TrimSpace(input)))
	}
	if redraws := strings.Count(output.String(), label); redraws != 1 {
		t.Fatalf("prompt render count after paste and arrows = %d, want 1", redraws)
	}
}
