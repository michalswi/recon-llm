package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
)

const (
	maxMasterNodes        = 32
	maxMasterResponseSize = 8 << 20
	masterMaxFailures     = 3

	masterNodeUnreachable AgentRunStatus = "unreachable"
	masterNodeRejected    AgentRunStatus = "rejected"
)

var (
	masterPollInterval   = 2 * time.Second
	masterRequestTimeout = 30 * time.Second
)

// masterNode is one remote recon API instance and the state of its current run.
// A node is only accessed from one goroutine at a time.
type masterNode struct {
	base     string
	label    string
	token    string
	client   *http.Client
	run      agentAPIRunResponse
	status   AgentRunStatus
	errText  string
	failures int
	shown    int
}

func newMasterHTTPClient() *http.Client {
	return &http.Client{
		// Never follow redirects: they could forward the bearer token elsewhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// parseMasterNodes validates a comma-separated list of recon API base URLs.
func parseMasterNodes(list, token string) ([]*masterNode, error) {
	seen := make(map[string]bool)
	var nodes []*masterNode
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, fmt.Errorf("invalid recon API URL %q: use http(s)://host:port", raw)
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, fmt.Errorf("invalid recon API URL %q: credentials, query and fragment are not allowed", raw)
		}
		base := strings.TrimRight(parsed.String(), "/")
		if seen[base] {
			continue
		}
		seen[base] = true
		if len(nodes) >= maxMasterNodes {
			return nil, fmt.Errorf("more than %d recon API URLs provided", maxMasterNodes)
		}
		nodes = append(nodes, &masterNode{
			base:   base,
			label:  parsed.Host + strings.TrimRight(parsed.Path, "/"),
			token:  token,
			client: newMasterHTTPClient(),
		})
	}
	if len(nodes) == 0 {
		return nil, errors.New("-master requires at least one recon API URL")
	}
	return nodes, nil
}

// sanitizeRemoteText drops control characters (including ESC) from text sent by a
// remote instance so it cannot manipulate the master's terminal.
func sanitizeRemoteText(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, text)
}

func (n *masterNode) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(ctx, masterRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, n.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if n.token != "" {
		request.Header.Set("Authorization", "Bearer "+n.token)
	}
	response, err := n.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMasterResponseSize+1))
	if err != nil {
		return err
	}
	if len(data) > maxMasterResponseSize {
		return fmt.Errorf("response exceeds %d bytes", maxMasterResponseSize)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var failure struct {
			Error string `json:"error"`
		}
		message := response.Status
		if json.Unmarshal(data, &failure) == nil && failure.Error != "" {
			message += ": " + failure.Error
		}
		return errors.New(sanitizeRemoteText(message))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (n *masterNode) health(ctx context.Context) error {
	return n.call(ctx, http.MethodGet, "/api/health", nil, nil)
}

func (n *masterNode) finished() bool {
	switch n.status {
	case agentCompleted, agentFailed, agentCancelled, agentLimitReached, masterNodeUnreachable, masterNodeRejected:
		return true
	}
	return false
}

func (n *masterNode) waiting() bool {
	switch n.status {
	case agentWaitingInput, agentWaitingApproval, agentWaitingContinue:
		return true
	}
	return false
}

func (n *masterNode) runPath(suffix string) string {
	return "/api/agent/runs/" + url.PathEscape(n.run.ID) + suffix
}

func (n *masterNode) update(run agentAPIRunResponse) {
	n.run = run
	n.status = run.Status
	n.failures = 0
	n.errText = ""
}

// fail records a request failure and gives up on the node after repeated failures.
func (n *masterNode) fail(err error) {
	n.failures++
	n.errText = err.Error()
	if n.failures >= masterMaxFailures {
		n.status = masterNodeUnreachable
	}
}

func (n *masterNode) act(ctx context.Context, suffix string, in any) {
	var run agentAPIRunResponse
	if err := n.call(ctx, http.MethodPost, n.runPath(suffix), in, &run); err != nil {
		n.fail(err)
		return
	}
	n.update(run)
}

// masterVerbose shows every node event (command output, plans, critic feedback,
// candidates). By default only commands, failed commands, warnings and errors are
// printed. It is read and written from the master's terminal goroutine only.
var masterVerbose bool

const masterOutputDisplayLimit = 1500

func masterFirstLine(text string) string {
	text = strings.TrimSpace(sanitizeRemoteText(text))
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return text[:i] + " ..."
	}
	return text
}

func (n *masterNode) printNewEvents(out io.Writer) {
	tag := "[" + n.label + "] "
	for ; n.shown < len(n.run.Events); n.shown++ {
		event := n.run.Events[n.shown]
		switch event.Kind {
		case "action":
			if event.Command != "" {
				fmt.Fprintf(out, "%s %s%s\n", runningLabel(), tag, sanitizeRemoteText(event.Command))
			} else if masterVerbose {
				fmt.Fprintf(out, "%s%s %s\n", info(tag+"action:"), event.Action, truncateMasterText(masterFirstLine(event.Text), 200))
			}
		case "observation":
			text := event.Output
			if text == "" {
				text = event.Text
			}
			switch {
			case masterVerbose && text != "":
				fmt.Fprintln(out, outputLabel(), tag+event.Action)
				fmt.Fprintln(out, truncateMasterText(strings.TrimSpace(sanitizeRemoteText(text)), masterOutputDisplayLimit))
			case !event.Success && text != "":
				fmt.Fprintln(out, warn(tag+event.Action+" failed:"), truncateMasterText(masterFirstLine(text), 300))
			}
		case "plan":
			if masterVerbose {
				fmt.Fprintln(out, accent(tag+"plan:"), truncateMasterText(strings.TrimSpace(sanitizeRemoteText(event.Text)), masterOutputDisplayLimit))
			}
		case "verification":
			if masterVerbose {
				fmt.Fprintln(out, info(tag+"critic requested revision:"), truncateMasterText(masterFirstLine(event.Text), 500))
			}
		case "candidate":
			if masterVerbose {
				fmt.Fprintln(out, info(tag+"rejected candidate:"), truncateMasterText(masterFirstLine(event.Text), 300))
			}
		case "clarification":
			if masterVerbose {
				fmt.Fprintln(out, accent(tag+"clarification:"), truncateMasterText(masterFirstLine(event.Text), 500))
			}
		case "warning":
			fmt.Fprintln(out, warn(tag+"Warning: "+sanitizeRemoteText(event.Text)))
		case "error", "invalid_action":
			fmt.Fprintln(out, warn(tag+"Agent error:"), formatAgentDisplayText(sanitizeRemoteText(event.Text)))
		}
	}
}

func (n *masterNode) lastClarification() string {
	for i := len(n.run.Events) - 1; i >= 0; i-- {
		if n.run.Events[i].Kind == "clarification" {
			return sanitizeRemoteText(n.run.Events[i].Text)
		}
	}
	return ""
}

// decide asks the user how to proceed with a node that is waiting for a decision.
func (n *masterNode) decide(ctx context.Context, reader *bufio.Reader, out io.Writer) error {
	label := "[" + n.label + "] "
	switch n.status {
	case agentWaitingInput:
		fmt.Fprintln(out, accent(label+"needs clarification:"), n.lastClarification())
		feedback, err := promptLine(reader, label+"Clarification: ")
		if err != nil {
			return err
		}
		if feedback == "" {
			fmt.Fprintln(out, warn("Clarification cannot be empty."))
			return nil
		}
		n.act(ctx, "/feedback", agentAPIFeedbackRequest{Feedback: feedback})
	case agentWaitingContinue:
		decision, err := promptLine(reader, label+"Iteration budget reached. Continue [c] or stop [s]: ")
		if err != nil {
			return err
		}
		switch strings.ToLower(decision) {
		case "c", "continue":
			n.act(ctx, "/continue", nil)
		case "s", "stop":
			n.act(ctx, "/cancel", nil)
		default:
			fmt.Fprintln(out, warn("Choose c or s."))
		}
	case agentWaitingApproval:
		fmt.Fprintln(out, separatorLine())
		fmt.Fprintln(out, assistantLabel(), accent(label+"candidate:"))
		fmt.Fprintln(out, sanitizeRemoteText(n.run.FinalAnswer))
		decision, err := promptLine(reader, label+"Accept [a], revise [r], or stop [s]: ")
		if err != nil {
			return err
		}
		switch strings.ToLower(decision) {
		case "a", "accept":
			n.act(ctx, "/approve", nil)
		case "r", "revise":
			feedback, err := promptLine(reader, label+"Revision feedback: ")
			if err != nil {
				return err
			}
			if feedback == "" {
				fmt.Fprintln(out, warn("Revision feedback cannot be empty."))
				return nil
			}
			n.act(ctx, "/feedback", agentAPIFeedbackRequest{Feedback: feedback})
		case "s", "stop":
			n.act(ctx, "/cancel", nil)
		default:
			fmt.Fprintln(out, warn("Choose a, r, or s."))
		}
	}
	if n.errText != "" && n.failures > 0 {
		fmt.Fprintln(out, warn("["+n.label+"] request failed:"), n.errText)
	}
	return nil
}

func masterCancelAll(nodes []*masterNode, out io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, node := range nodes {
		if node.finished() || node.run.ID == "" {
			continue
		}
		wg.Add(1)
		go func(node *masterNode) {
			defer wg.Done()
			node.act(ctx, "/cancel", nil)
		}(node)
	}
	wg.Wait()
	for _, node := range nodes {
		if node.status == agentCancelled {
			fmt.Fprintln(out, warn("["+node.label+"] run cancelled."))
		}
	}
}

func forEachMasterNode(nodes []*masterNode, fn func(*masterNode)) {
	var wg sync.WaitGroup
	for _, node := range nodes {
		wg.Add(1)
		go func(node *masterNode) {
			defer wg.Done()
			fn(node)
		}(node)
	}
	wg.Wait()
}

// runMasterGoal sends goal to every node, relays decisions to the user, and
// prints an aggregated summary. Nodes run independently; one failing node does
// not stop the others.
func runMasterGoal(ctx context.Context, reader *bufio.Reader, nodes []*masterNode, goal string, out io.Writer) error {
	for _, node := range nodes {
		*node = masterNode{base: node.base, label: node.label, token: node.token, client: node.client}
	}
	forEachMasterNode(nodes, func(node *masterNode) {
		var run agentAPIRunResponse
		if err := node.call(ctx, http.MethodPost, "/api/agent/runs", AgentRequest{Goal: goal}, &run); err != nil {
			node.status = masterNodeRejected
			node.errText = err.Error()
			return
		}
		node.update(run)
	})
	for _, node := range nodes {
		if node.status == masterNodeRejected {
			fmt.Fprintln(out, warn("["+node.label+"] could not start run:"), node.errText)
		} else {
			fmt.Fprintln(out, info("["+node.label+"] run "+sanitizeRemoteText(node.run.ID)+" started"))
		}
	}

	for {
		if ctx.Err() != nil {
			masterCancelAll(nodes, out)
			fmt.Fprintln(out, warn("Master stopped."))
			return nil
		}
		allFinished := true
		for _, node := range nodes {
			if !node.finished() {
				allFinished = false
			}
		}
		if allFinished {
			break
		}

		forEachMasterNode(nodes, func(node *masterNode) {
			if node.status != agentRunning {
				return
			}
			var run agentAPIRunResponse
			if err := node.call(ctx, http.MethodGet, node.runPath(""), nil, &run); err != nil {
				if ctx.Err() == nil {
					node.fail(err)
				}
				return
			}
			node.update(run)
		})
		prompted := false
		for _, node := range nodes {
			node.printNewEvents(out)
			if node.status == masterNodeUnreachable && node.errText != "" {
				fmt.Fprintln(out, warn("["+node.label+"] unreachable:"), node.errText)
				node.errText = ""
			}
			if !node.waiting() || ctx.Err() != nil {
				continue
			}
			prompted = true
			if err := node.decide(ctx, reader, out); err != nil {
				masterCancelAll(nodes, out)
				return err
			}
			node.printNewEvents(out)
		}
		if !prompted {
			select {
			case <-ctx.Done():
			case <-time.After(masterPollInterval):
			}
		}
	}
	printMasterSummary(nodes, out)
	return nil
}

func printMasterSummary(nodes []*masterNode, out io.Writer) {
	fmt.Fprintln(out, separatorLine())
	fmt.Fprintln(out, accent("Summary:"))
	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, node := range nodes {
		detail := ""
		if node.status != agentCompleted {
			detail = node.run.Error
			if detail == "" {
				detail = node.errText
			}
		}
		fmt.Fprintf(writer, "  %s\t%s\titerations: %d\t%s\n", node.label, node.status, node.run.Iteration, sanitizeRemoteText(detail))
	}
	writer.Flush()
	for _, node := range nodes {
		if node.status == agentCompleted && node.run.FinalAnswer != "" {
			fmt.Fprintln(out, separatorLine())
			fmt.Fprintln(out, accent("["+node.label+"]"))
			fmt.Fprintln(out, sanitizeRemoteText(node.run.FinalAnswer))
		}
	}
}

func printMasterNodes(ctx context.Context, nodes []*masterNode, out io.Writer) {
	errs := make([]error, len(nodes))
	var wg sync.WaitGroup
	for i, node := range nodes {
		wg.Add(1)
		go func(i int, node *masterNode) {
			defer wg.Done()
			errs[i] = node.health(ctx)
		}(i, node)
	}
	wg.Wait()
	for i, node := range nodes {
		if errs[i] != nil {
			fmt.Fprintln(out, warn("  "+node.label+": unreachable:"), errs[i])
		} else {
			fmt.Fprintln(out, success("  "+node.label+": ok"))
		}
	}
}

func printMasterHelp() {
	fmt.Println(accent("Master commands:"))
	fmt.Println("  <objective>        The orchestrator (this instance's Ollama model) splits it into subtasks,")
	fmt.Println("                     dispatches them to nodes, reviews the results and merges one answer")
	fmt.Println("  /broadcast GOAL    Send the same goal to every node, without an orchestrator model")
	fmt.Println("  /nodes             Check which nodes are reachable (and that the token is accepted)")
	fmt.Println("  /verbose           Toggle full node activity: command output, plans, critic feedback")
	fmt.Println("  /m                 Show the orchestrator model")
	fmt.Println("  /h, /help          Show this help")
	fmt.Println("  /q                 Exit; Ctrl+C cancels the active runs on all nodes")
	fmt.Println("Each node runs its own worker, critic and limits. Every dispatch needs your approval")
	fmt.Println("(or [a] for all of the current objective), and the merged answer is verified and approved by you.")
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func warnIfInsecureMasterNodes(nodes []*masterNode) {
	for _, node := range nodes {
		parsed, err := url.Parse(node.base)
		if err != nil || parsed.Scheme != "http" || isLoopbackHost(parsed.Hostname()) {
			continue
		}
		fmt.Println(warn(fmt.Sprintf("Warning: %s uses plain HTTP to a non-loopback host; goals, results and the RECON_API_TOKEN bearer token cross the network unencrypted. Use https:// via a trusted proxy or an encrypted tunnel.", node.base)))
	}
}

// readMasterLine is readGoalLine except that end of input is reported as io.EOF
// so the master loop can exit instead of re-prompting forever on a closed stdin.
func readMasterLine(reader *bufio.Reader) (string, error) {
	if line, err, ok := readInteractiveLine(reader, goalLabel("[goal]: ")); ok {
		return line, err
	}
	fmt.Print(goalLabel("[goal]: "))
	line, err := reader.ReadString('\n')
	return strings.TrimSpace(line), err
}

// runMaster is the terminal loop of master mode: it orchestrates remote recon
// API instances instead of running an agent locally.
func runMaster(list string) error {
	nodes, err := parseMasterNodes(list, agentAPIToken)
	if err != nil {
		return err
	}
	ShowBanner()
	warnIfInsecureOllamaURL(ollamaURL)
	warnIfInsecureOllamaTLS()
	warnIfInsecureMasterNodes(nodes)
	if agentAPIToken == "" {
		fmt.Println(info("RECON_API_TOKEN is not set; no Authorization header will be sent."))
	}
	fmt.Println(info(fmt.Sprintf("Master mode: %d node(s); orchestrator model %s", len(nodes), modelName)))
	checkContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	printMasterNodes(checkContext, nodes, os.Stdout)
	if message, err := checkOllamaReachable(checkContext, ollamaURL); err != nil {
		fmt.Println(warn("Ollama unreachable (the orchestrator needs it; /broadcast does not):"), err)
	} else {
		fmt.Println(success(message))
	}
	cancel()
	fmt.Println(info("Enter an objective for the orchestrator, or /broadcast GOAL for every node. Use /h for help.\n"))

	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := readMasterLine(reader)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			if errors.Is(err, io.EOF) {
				return nil
			}
			continue
		}
		switch strings.ToLower(line) {
		case "exit", "quit", "/q":
			return nil
		case "/h", "/help":
			printMasterHelp()
			continue
		case "/m":
			fmt.Println(info("Model: " + modelName))
			continue
		case "/verbose":
			masterVerbose = !masterVerbose
			if masterVerbose {
				fmt.Println(info("Verbose on: showing command output, plans and critic feedback from nodes."))
			} else {
				fmt.Println(info("Verbose off: showing commands, failed commands, warnings and errors."))
			}
			continue
		case "/nodes":
			checkContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			printMasterNodes(checkContext, nodes, os.Stdout)
			cancel()
			continue
		}
		broadcast := false
		if fields := strings.Fields(line); strings.EqualFold(fields[0], "/broadcast") {
			line = strings.TrimSpace(line[len(fields[0]):])
			if line == "" {
				fmt.Println(warn("Usage: /broadcast GOAL"))
				continue
			}
			broadcast = true
		} else if strings.HasPrefix(line, "/") {
			fmt.Println(warn("Unknown command:"), line)
			continue
		}
		signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		if !broadcast {
			preflight, cancelPreflight := context.WithTimeout(signalContext, 5*time.Second)
			_, ollamaErr := checkOllamaReachable(preflight, ollamaURL)
			cancelPreflight()
			if ollamaErr != nil && signalContext.Err() == nil {
				stop()
				fmt.Println(warn("The orchestrator needs its own Ollama model, but "+ollamaURL+" is unreachable:"), ollamaErr)
				fmt.Println(info("The master does not use the nodes' Ollama. Set OLLAMA_URL and OLLAMA_MODEL for the master process, or use /broadcast GOAL to send a goal to every node without an orchestrator."))
				continue
			}
		}
		if broadcast {
			err = runMasterGoal(signalContext, reader, nodes, line, os.Stdout)
		} else {
			err = orchestrateMaster(signalContext, reader, nodes, line, os.Stdout)
		}
		stop()
		if err != nil {
			fmt.Println(warn("Master run failed:"), err)
		}
	}
}
