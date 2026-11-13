package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

func printHelp() {
	fmt.Println(accent("Agent commands:"))
	commands := [][2]string{
		{"<objective>", "Type a task to start immediately, without a form"},
		{"/agent [GOAL]", "Start a guided task form; GOAL is optional (see examples below)"},
		{"/h, /help", "Show this help"},
		{"/q", "Exit; Ctrl+C cancels an active run"},
		{"/dir", "Show current working directory"},
		{"/m", "Show the Ollama model"},
		{"/up", "Show app uptime"},
		{"/ollama", "Check whether the Ollama server is reachable"},
		{"/ollama-ps", "Show models currently loaded in Ollama"},
		{"/ollama-ls", "Show models available in Ollama"},
		{"/attach PATH", "Attach a file or directory for subsequent runs"},
		{"/attached", "List currently attached files"},
		{"/deattach [PATH]", "Clear all persistent attachments, or just PATH (/detach also works)"},
		{"/sysprompt", "Show the agent base prompt"},
		{"/editsysprompt", "Select or enter an agent base prompt"},
		{"/skills", "List external skills from ~/.recon/skills"},
		{"/think-on", "Enable model reasoning"},
		{"/think-off", "Disable model reasoning"},
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, command := range commands {
		fmt.Fprintf(writer, "  %s\t%s\n", command[0], command[1])
	}
	writer.Flush()
	fmt.Println(accent("\nStarting a task:"))
	fmt.Println("  Explain DNS         Start immediately with this goal.")
	fmt.Println("  /agent              Ask for the goal, then optional details.")
	fmt.Println("  /agent Explain DNS  Use this goal, then ask for optional details.")
	fmt.Println("  [GOAL] means optional text; do not type the brackets.")
	fmt.Println("  The form asks for expected output, constraints, completion criteria,")
	fmt.Println("  an attachment, a folder where recon may write files, and skills.")
	fmt.Println("  Press Enter to skip any optional field. Both ways use the same agent.")
	fmt.Println()
	fmt.Println(accent("Agent tool allowlist:"))
	fmt.Printf("  [%s]\n", strings.Join(allowedCommandsList(), ", "))
}

func printSystemPrompt() {
	fmt.Println(accent("Current agent base prompt:"))
	fmt.Println(systemPrompt)
}

func handleEditSystemPrompt() {
	files, _ := embeddedPrompts.ReadDir("prompts")
	var promptFiles []string
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".txt") && file.Name() != "agent.txt" {
			promptFiles = append(promptFiles, file.Name())
		}
	}
	sort.Strings(promptFiles)

	fmt.Println(accent("Available base prompts:"))
	for index, name := range promptFiles {
		fmt.Printf("  %d. %s\n", index+1, name)
	}
	fmt.Println("  custom: enter a custom base prompt")
	fmt.Println("  cancel: keep the current prompt")

	reader := bufio.NewReader(os.Stdin)
	choice, err := promptLine(reader, "Choose option: ")
	if err != nil || strings.EqualFold(choice, "cancel") {
		return
	}
	if strings.EqualFold(choice, "custom") {
		fmt.Println(info("Enter the prompt; finish with a line containing only EOF."))
		customPrompt, err := readUntilDelimiter(reader, "EOF")
		if err != nil {
			fmt.Println(warn("Read error:"), err)
			return
		}
		customPrompt = strings.TrimSpace(customPrompt)
		if customPrompt == "" {
			fmt.Println(warn("Prompt cannot be empty."))
			return
		}
		systemPrompt = customPrompt
		systemPromptName = "custom"
		return
	}

	index, err := strconv.Atoi(choice)
	if err != nil || index < 1 || index > len(promptFiles) {
		fmt.Println(warn("Invalid selection."))
		return
	}
	name := promptFiles[index-1]
	data, err := embeddedPrompts.ReadFile(filepath.Join("prompts", name))
	if err != nil {
		fmt.Println(warn("Read error:"), err)
		return
	}
	systemPrompt = string(data)
	systemPromptName = name
	fmt.Println(success("Agent base prompt selected: " + name))
}

func readUntilDelimiter(reader *bufio.Reader, delimiter string) (string, error) {
	var content strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		if strings.TrimRight(line, "\r\n") == delimiter {
			return content.String(), nil
		}
		content.WriteString(line)
		if errors.Is(err, io.EOF) {
			return content.String(), nil
		}
	}
}
