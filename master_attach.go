package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	masterPromptExcerpt      = 600
	masterPromptExcerptTotal = 4 << 10
)

// masterAttachment is a local file the master read so it can send the contents to
// nodes, which cannot access the master's filesystem. Text is a snapshot taken
// when the file was attached, capped at maxAgentAttachmentText.
type masterAttachment struct {
	Path      string
	Name      string
	Size      int64
	Text      string
	Truncated bool
	// FromDir marks files attached through a directory, which get a smaller
	// inline excerpt, as in recon itself.
	FromDir bool
	// Note explains why no text is available (extraction failed or file too large).
	Note string
}

// inlineLimit is how much of a file's text one goal may carry: the full budget for
// a file attached directly, a short excerpt for files attached through a directory.
func (a masterAttachment) inlineLimit() int {
	if a.FromDir {
		return maxAgentDirectoryExcerpt
	}
	return maxAgentAttachmentText
}

// sentBytes is how many bytes of this file's text a goal carries, ignoring the
// shared total budget.
func (a masterAttachment) sentBytes() int { return min(len(a.Text), a.inlineLimit()) }

func (a masterAttachment) partial() bool {
	return a.Truncated || len(a.Text) > a.inlineLimit()
}

// masterAttachments are the files attached in this master session. They are only
// accessed from the master's terminal goroutine (and copied by orchestrators).
var masterAttachments []masterAttachment

func loadMasterAttachment(path string, fromDir bool) (masterAttachment, error) {
	info, err := os.Stat(path)
	if err != nil {
		return masterAttachment{}, fmt.Errorf("open attachment: %w", err)
	}
	if !info.Mode().IsRegular() {
		return masterAttachment{}, errors.New("attachment must be a regular file")
	}
	if isSupportedImagePath(path) {
		return masterAttachment{}, errors.New("images are not supported in master mode: nodes can only receive text from the master")
	}
	file := masterAttachment{Path: path, Name: filepath.Base(path), Size: info.Size(), FromDir: fromDir}
	text, err := extractAttachmentText(path, info.Size())
	if err != nil {
		// Like recon, keep the file listed with metadata when no text is available.
		file.Note = "no text content is available (" + err.Error() + ")"
		return file, nil
	}
	text = strings.ToValidUTF8(text, "")
	if len(text) > maxAgentAttachmentText {
		text, file.Truncated = text[:maxAgentAttachmentText], true
	}
	file.Text = text
	return file, nil
}

// masterFileContents converts files into the API's fileContents field. The total
// is bounded by maxAgentAttachmentText, the same budget recon uses locally, and
// the node enforces it again. The second result reports whether anything was cut.
func masterFileContents(files []masterAttachment) ([]AgentFileContent, bool) {
	contents := make([]AgentFileContent, 0, len(files))
	remaining := maxAgentAttachmentText
	anyTruncated := false
	for _, file := range files {
		excerpt, cut := attachmentTextExcerpt(file.Text, file.inlineLimit(), remaining)
		remaining -= len(excerpt)
		item := AgentFileContent{Name: file.Name, Size: file.Size, Content: excerpt, Note: file.Note}
		if file.Note == "" && (cut || file.partial() || excerpt == "") {
			item.Truncated = true
			anyTruncated = true
		}
		contents = append(contents, item)
	}
	return contents, anyTruncated
}

func masterAttachmentsSummary(files []masterAttachment) string {
	var summary strings.Builder
	for _, file := range files {
		fmt.Fprintf(&summary, "%s (%d bytes%s)\n", file.Name, file.Size, masterAttachmentStatus(file))
	}
	return strings.TrimSpace(summary.String())
}

// masterAttachmentStatus describes how much of the file a goal carries.
func masterAttachmentStatus(file masterAttachment) string {
	switch {
	case file.Note != "":
		return ", no text available"
	case file.partial():
		return fmt.Sprintf(", first %d KiB of text sent", max(1, file.sentBytes()>>10))
	}
	return ""
}

// masterAttachmentsPromptText gives the orchestrator names, sizes and short
// excerpts so it can plan; full content is added to a goal only through the
// dispatch "files" field.
func masterAttachmentsPromptText(files []masterAttachment) string {
	if len(files) == 0 {
		return "No files attached."
	}
	var text strings.Builder
	remaining := masterPromptExcerptTotal
	for _, file := range files {
		excerpt, truncated := attachmentTextExcerpt(file.Text, masterPromptExcerpt, remaining)
		remaining -= len(excerpt)
		fmt.Fprintf(&text, "- %s (%d bytes%s)", file.Name, file.Size, masterAttachmentStatus(file))
		if excerpt != "" {
			fmt.Fprintf(&text, ", beginning:\n%s", strings.TrimSpace(excerpt))
			if truncated || len(file.Text) > len(excerpt) {
				text.WriteString("\n...")
			}
		}
		text.WriteString("\n")
	}
	return strings.TrimSpace(text.String())
}

func findMasterAttachments(files []masterAttachment, names []string) ([]masterAttachment, error) {
	var selected []masterAttachment
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if seen[name] {
			continue
		}
		seen[name] = true
		found := false
		for _, file := range files {
			if file.Name == name {
				selected = append(selected, file)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown file %q", name)
		}
	}
	return selected, nil
}

// handleMasterAttachCommand implements /attach, /attached and /detach for the
// master. It reports whether input was one of those commands.
func handleMasterAttachCommand(input string) bool {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return false
	}
	argument := strings.TrimSpace(input[len(parts[0]):])
	switch strings.ToLower(parts[0]) {
	case "/attach":
		paths, err := resolveAttachmentPaths(argument)
		if err != nil {
			fmt.Println(warn("Attachment error:"), err)
			return true
		}
		fromDir := false
		if absolute, err := filepath.Abs(argument); err == nil {
			if stat, err := os.Stat(absolute); err == nil && stat.IsDir() {
				fromDir = true
			}
		}
		updated := append([]masterAttachment(nil), masterAttachments...)
		var added []string
		for _, path := range paths {
			already := false
			for _, file := range updated {
				if file.Path == path {
					already = true
				} else if file.Name == filepath.Base(path) {
					fmt.Println(warn("Attachment error:"), fmt.Sprintf("a different file named %q is already attached; detach it first", file.Name))
					already = true
				}
			}
			if already {
				continue
			}
			if len(updated) >= maxAgentAttachments {
				fmt.Printf("%s maximum is %d files.\n", warn("Attachment error:"), maxAgentAttachments)
				break
			}
			file, err := loadMasterAttachment(path, fromDir)
			if err != nil {
				fmt.Println(warn("Skipped "+path+":"), err)
				continue
			}
			updated = append(updated, file)
			added = append(added, file.Name+masterAttachmentStatusLabel(file))
		}
		if len(added) == 0 {
			fmt.Println(warn("Attachment error:"), "no new files were attached")
			return true
		}
		masterAttachments = updated
		fmt.Printf("%s %s\n", success("Attached:"), strings.Join(added, ", "))
		fmt.Println(info("Contents are read now and sent to the nodes (and their models) with each run; do not attach secrets. Limits as in recon: 64 KiB of text for a file attached directly, 1 KiB per file from a directory, 64 KiB per goal in total. Nodes cannot read the rest of a file; re-attach to refresh a changed file."))
	case "/attached":
		if len(masterAttachments) == 0 {
			fmt.Println(info("No attached files."))
			return true
		}
		fmt.Println(accent("Attached files (contents are sent to nodes):"))
		for _, file := range masterAttachments {
			fmt.Printf("  %s (%d bytes%s)  %s\n", file.Name, file.Size, masterAttachmentStatus(file), file.Path)
		}
	case "/detach", "/deattach":
		if argument == "" {
			masterAttachments = nil
			fmt.Println(info("Attachments cleared."))
			return true
		}
		absolutePath, err := filepath.Abs(argument)
		if err != nil {
			fmt.Println(warn("Attachment error:"), err)
			return true
		}
		updated := make([]masterAttachment, 0, len(masterAttachments))
		removed := false
		for _, file := range masterAttachments {
			if file.Path == absolutePath || file.Name == argument {
				removed = true
				continue
			}
			updated = append(updated, file)
		}
		if !removed {
			fmt.Printf("%s %q is not attached.\n", warn("Attachment error:"), argument)
			return true
		}
		masterAttachments = updated
		fmt.Printf("%s %d file(s) remain attached.\n", info("Detached:"), len(masterAttachments))
	default:
		return false
	}
	return true
}

func masterAttachmentStatusLabel(file masterAttachment) string {
	if status := masterAttachmentStatus(file); status != "" {
		return " (" + strings.TrimPrefix(status, ", ") + ")"
	}
	return ""
}
