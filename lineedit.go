package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const maxPromptHistoryEntries = 500

type promptHistory struct {
	mu      sync.Mutex
	entries []string
	path    string
}

var (
	sharedPromptHistory     *promptHistory
	sharedPromptHistoryOnce sync.Once
)

func loadPromptHistory() *promptHistory {
	sharedPromptHistoryOnce.Do(func() {
		sharedPromptHistory = &promptHistory{}
		if path, err := reconHistoryPath(); err == nil {
			sharedPromptHistory.path = path
			if data, readErr := os.ReadFile(path); readErr == nil {
				lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
				for _, line := range lines {
					if line != "" {
						sharedPromptHistory.entries = append(sharedPromptHistory.entries, line)
					}
				}
				if len(sharedPromptHistory.entries) > maxPromptHistoryEntries {
					sharedPromptHistory.entries = sharedPromptHistory.entries[len(sharedPromptHistory.entries)-maxPromptHistoryEntries:]
				}
			}
		}
	})
	return sharedPromptHistory
}

func reconHistoryPath() (string, error) {
	baseDirectory, err := reconHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(baseDirectory, "history"), nil
}

func (h *promptHistory) add(line string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.entries) > 0 && h.entries[len(h.entries)-1] == line {
		return
	}
	h.entries = append(h.entries, line)
	if len(h.entries) > maxPromptHistoryEntries {
		h.entries = h.entries[len(h.entries)-maxPromptHistoryEntries:]
	}
	if h.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0755); err != nil {
		return
	}
	file, err := os.OpenFile(h.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer file.Close()
	fmt.Fprintln(file, line)
}

// readInteractiveLine provides arrow-key cursor movement and up/down history
// navigation when stdin is a real terminal. ok is false when raw mode is
// unavailable (piped input, non-interactive stdin, unsupported platform), in
// which case the caller must fall back to plain line reading.
func readInteractiveLine(reader *bufio.Reader, label string) (line string, err error, ok bool) {
	fd := int(os.Stdin.Fd())
	restore, rawErr := platformEnableRawMode(fd)
	if rawErr != nil {
		return "", nil, false
	}
	defer restore()

	line, err = runLineEditorWithOutputAndWidth(reader, label, loadPromptHistory(), os.Stdout, platformTerminalWidth(fd))
	return line, err, true
}

// runLineEditor implements the escape-sequence and history parsing loop; it is
// independent of raw-mode setup so it can be exercised with a plain byte reader.
func runLineEditor(reader *bufio.Reader, label string, history *promptHistory) (string, error) {
	return runLineEditorWithOutput(reader, label, history, os.Stdout)
}

func runLineEditorWithOutput(reader *bufio.Reader, label string, history *promptHistory, output io.Writer) (string, error) {
	return runLineEditorWithOutputAndWidth(reader, label, history, output, 80)
}

func runLineEditorWithOutputAndWidth(reader *bufio.Reader, label string, history *promptHistory, output io.Writer, terminalWidth int) (string, error) {
	history.mu.Lock()
	historyLen := len(history.entries)
	history.mu.Unlock()
	if terminalWidth <= 0 {
		terminalWidth = 80
	}

	buffer := make([]byte, 0, 64)
	cursor := 0
	historyIndex := historyLen
	var draft []byte
	labelWidth := visibleTerminalWidth(label)
	renderedRows := 1
	renderedCursorRow := 0

	redraw := func() {
		fmt.Fprint(output, "\r")
		if renderedCursorRow > 0 {
			fmt.Fprintf(output, "\x1b[%dA", renderedCursorRow)
		}
		for row := 0; row < renderedRows; row++ {
			fmt.Fprint(output, "\x1b[2K")
			if row < renderedRows-1 {
				fmt.Fprint(output, "\x1b[1B\r")
			}
		}
		if renderedRows > 1 {
			fmt.Fprintf(output, "\x1b[%dA", renderedRows-1)
		}
		fmt.Fprint(output, "\r")
		fmt.Fprint(output, label)
		fmt.Fprint(output, string(buffer))
		renderedRows = terminalRows(labelWidth+len(buffer), terminalWidth)
		endRow, _ := terminalPosition(labelWidth+len(buffer), terminalWidth)
		cursorRow, cursorColumn := terminalPosition(labelWidth+cursor, terminalWidth)
		fmt.Fprint(output, "\r")
		if moveUp := endRow - cursorRow; moveUp > 0 {
			fmt.Fprintf(output, "\x1b[%dA", moveUp)
		}
		if cursorColumn > 0 {
			fmt.Fprintf(output, "\x1b[%dC", cursorColumn)
		}
		renderedCursorRow = cursorRow
	}
	redraw()

	for {
		b, readErr := reader.ReadByte()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return strings.TrimSpace(string(buffer)), io.EOF
			}
			return "", readErr
		}
		switch b {
		case '\r', '\n':
			fmt.Fprint(output, "\r\n")
			result := strings.TrimSpace(string(buffer))
			if result != "" {
				history.add(result)
			}
			return result, nil
		case 3: // Ctrl+C
			fmt.Fprint(output, "\r\n")
			return "", io.EOF
		case 4: // Ctrl+D
			if len(buffer) == 0 {
				fmt.Fprint(output, "\r\n")
				return "", io.EOF
			}
		case 127, 8: // Backspace
			if cursor > 0 {
				buffer = append(buffer[:cursor-1], buffer[cursor:]...)
				cursor--
				redraw()
			}
		case 27: // ESC: possible arrow-key sequence
			second, secondErr := reader.ReadByte()
			if secondErr != nil || second != '[' {
				continue
			}
			third, thirdErr := reader.ReadByte()
			if thirdErr != nil {
				continue
			}
			history.mu.Lock()
			entries := append([]string(nil), history.entries...)
			history.mu.Unlock()
			switch third {
			case 'A': // Up
				if len(entries) == 0 {
					continue
				}
				if historyIndex == len(entries) {
					draft = append([]byte(nil), buffer...)
				}
				if historyIndex > 0 {
					historyIndex--
				}
				buffer = []byte(entries[historyIndex])
				cursor = len(buffer)
				redraw()
			case 'B': // Down
				if historyIndex < len(entries) {
					historyIndex++
				}
				if historyIndex >= len(entries) {
					historyIndex = len(entries)
					buffer = append([]byte(nil), draft...)
				} else {
					buffer = []byte(entries[historyIndex])
				}
				cursor = len(buffer)
				redraw()
			case 'C': // Right
				if cursor < len(buffer) {
					cursor++
					moveEditorCursor(output, labelWidth+cursor, terminalWidth, &renderedCursorRow)
				}
			case 'D': // Left
				if cursor > 0 {
					cursor--
					moveEditorCursor(output, labelWidth+cursor, terminalWidth, &renderedCursorRow)
				}
			}
		default:
			if b >= 32 {
				if cursor == len(buffer) {
					buffer = append(buffer, b)
					cursor++
					fmt.Fprintf(output, "%c", b)
					renderedRows = terminalRows(labelWidth+len(buffer), terminalWidth)
					renderedCursorRow, _ = terminalPosition(labelWidth+cursor, terminalWidth)
					continue
				}
				buffer = append(buffer[:cursor], append([]byte{b}, buffer[cursor:]...)...)
				cursor++
				redraw()
			}
		}
	}
}

func terminalRows(contentWidth, terminalWidth int) int {
	if contentWidth <= 0 {
		return 1
	}
	return (contentWidth-1)/terminalWidth + 1
}

func terminalPosition(contentWidth, terminalWidth int) (row, column int) {
	return contentWidth / terminalWidth, contentWidth % terminalWidth
}

func moveEditorCursor(output io.Writer, contentWidth, terminalWidth int, currentRow *int) {
	targetRow, targetColumn := terminalPosition(contentWidth, terminalWidth)
	fmt.Fprint(output, "\r")
	if moveUp := *currentRow - targetRow; moveUp > 0 {
		fmt.Fprintf(output, "\x1b[%dA", moveUp)
	} else if moveDown := targetRow - *currentRow; moveDown > 0 {
		fmt.Fprintf(output, "\x1b[%dB", moveDown)
	}
	if targetColumn > 0 {
		fmt.Fprintf(output, "\x1b[%dC", targetColumn)
	}
	*currentRow = targetRow
}

func visibleTerminalWidth(text string) int {
	width := 0
	for index := 0; index < len(text); {
		if text[index] == '\x1b' && index+1 < len(text) && text[index+1] == '[' {
			index += 2
			for index < len(text) && (text[index] < '@' || text[index] > '~') {
				index++
			}
			if index < len(text) {
				index++
			}
			continue
		}
		width++
		index++
	}
	return width
}
