package main

import (
	"embed"
	"strconv"
	"strings"
	"time"

	"github.com/michalswi/recon/utils"
)

const (
	separator = "------------------------------------------------------------"
)

//go:embed prompts/*.txt
var embeddedPrompts embed.FS

var version = "v0.0.1"

var defaultSystemPrompt = `
You are a helpful, general-purpose assistant.

Default behavior: answer the user's question.

Only when the user explicitly and strictly asks you to execute a shell command (e.g., "execute this command:", "run:", "run this command", "run the following"), respond ONLY as JSON:
{"action": "run_command", "command": "<shell command>"}

In all other cases, respond ONLY as JSON:
{"action": "answer", "text": "<your normal answer>"}

Never include backticks, comments, or extra keys.
`

var ollamaURL = utils.GetEnv("OLLAMA_URL", "http://localhost:11434/api/chat")
var ollamaCACertPath = utils.GetEnv("OLLAMA_CA_CERT", "")
var ollamaInsecureSkipVerify = strings.EqualFold(utils.GetEnv("OLLAMA_INSECURE_SKIP_VERIFY", ""), "true")
var modelName = utils.GetEnv("OLLAMA_MODEL", "qwen3.5:0.8b")

// ollamaNumPredict defaults to -1 (Ollama's "no limit", generate until a stop
// token or the context is full) so a model whose Modelfile bakes in a smaller
// default can't silently truncate long answers (e.g. generated code) mid-JSON.
var ollamaNumPredict = parseIntEnv("OLLAMA_NUM_PREDICT", -1)

// ollamaNumCtx is 0 (omitted, use the model/server default) unless set.
var ollamaNumCtx = parseIntEnv("OLLAMA_NUM_CTX", 0)

func parseIntEnv(key string, defaultValue int) int {
	value := utils.GetEnv(key, "")
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return parsed
}
var systemPrompt, systemPromptName = utils.LoadSystemPromptWithName(defaultSystemPrompt)
var agentAPIAddress = utils.GetEnv("RECON_API_ADDRESS", "")

var thinkingEnabled bool
var startTime time.Time
