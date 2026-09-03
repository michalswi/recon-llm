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

	line, err = runLineEditor(reader, label, loadPromptHistory())
	return line, err, true
}

// runLineEditor implements the escape-sequence and history parsing loop; it is
// independent of raw-mode setup so it can be exercised with a plain byte reader.
func runLineEditor(reader *bufio.Reader, label string, history *promptHistory) (string, error) {
	history.mu.Lock()
	historyLen := len(history.entries)
	history.mu.Unlock()

	buffer := make([]byte, 0, 64)
	cursor := 0
	historyIndex := historyLen
	var draft []byte

	redraw := func() {
		fmt.Print("\r\x1b[K")
		fmt.Print(label)
		fmt.Print(string(buffer))
		if moveLeft := len(buffer) - cursor; moveLeft > 0 {
			fmt.Printf("\x1b[%dD", moveLeft)
		}
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
			fmt.Print("\r\n")
			result := strings.TrimSpace(string(buffer))
			if result != "" {
				history.add(result)
			}
			return result, nil
		case 3: // Ctrl+C
			fmt.Print("\r\n")
			return "", io.EOF
		case 4: // Ctrl+D
			if len(buffer) == 0 {
				fmt.Print("\r\n")
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
					redraw()
				}
			case 'D': // Left
				if cursor > 0 {
					cursor--
					redraw()
				}
			}
		default:
			if b >= 32 {
				buffer = append(buffer[:cursor], append([]byte{b}, buffer[cursor:]...)...)
				cursor++
				redraw()
			}
		}
	}
}
