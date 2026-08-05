// Command llm is a standalone test client for ongrid's LLM API surface.
// It exercises every LLM-related HTTP endpoint that ongrid exposes —
// chat sessions, streaming messages, LLM configuration, model catalog,
// RAG knowledge, skills, agents, MCP servers, query translation, and
// token usage — without adding any new server-side code.
//
// Usage:
//
//	llm [flags] <command> [args]
//
// Flags:
//
//	--addr   ongrid base URL (default http://localhost:8080)
//	--email  login email (default admin@ongrid.local)
//	--pass   login password (default change-me-on-first-login)
//
// Commands:
//
//	login          POST /v1/auth/login → bearer token
//	self           GET  /v1/self
//
//	# LLM configuration
//	llm-test       POST /v1/integrations/llm/test
//	llm-save       POST /v1/integrations/llm/validate-and-save
//	llm-invalidate POST /v1/integrations/llm/invalidate
//	llm-settings   GET  /v1/system-settings?category=llm
//	llm-setting    PUT  /v1/system-settings/llm/<key>
//
//	# Model catalog
//	models         GET  /v1/aiops/models
//	usage          GET  /v1/usage/today
//
//	# Chat sessions
//	session-create POST   /v1/chat/sessions
//	session-list   GET    /v1/chat/sessions
//	session-rename PATCH  /v1/chat/sessions/<id>
//	session-close  DELETE /v1/chat/sessions/<id>
//
//	# Messages
//	chat           POST  /v1/chat/sessions/<id>/messages
//	chat-stream    POST  /v1/chat/sessions/<id>/messages/stream
//	chat-stop      POST  /v1/chat/sessions/<id>/stop
//	messages       GET   /v1/chat/sessions/<id>/messages
//
//	# Query translation
//	translate      POST  /v1/aiops/query-translate
//
//	# Agents
//	agent-list     GET    /v1/agents
//	agent-get      GET    /v1/agents/<name>
//	agent-create   POST   /v1/agents/custom
//	agent-update   PATCH  /v1/agents/custom/<name>
//	agent-delete   DELETE /v1/agents/custom/<name>
//
//	# Skills
//	skill-list     GET  /v1/skills
//	skill-get      GET  /v1/skills/<key>
//	skill-exec     POST /v1/skills/<key>/execute
//
//	# Knowledge / RAG
//	doc-list       GET    /v1/knowledge/docs
//	doc-get        GET    /v1/knowledge/docs/<id>
//	doc-create     POST   /v1/knowledge/docs
//	doc-update     PATCH  /v1/knowledge/docs/<id>
//	doc-delete     DELETE /v1/knowledge/docs/<id>
//	doc-upload     POST   /v1/knowledge/upload
//	doc-move       PATCH  /v1/knowledge/docs/<id>/move
//	kn-search      GET    /v1/knowledge/search
//	kn-paths       GET    /v1/knowledge/paths
//	repo-list      GET    /v1/knowledge/repos
//	repo-create    POST   /v1/knowledge/repos
//	repo-sync      POST   /v1/knowledge/repos/<id>/sync
//	repo-delete    DELETE /v1/knowledge/repos/<id>
//	vault-sync     POST   /v1/knowledge/vault/sync
//
//	# MCP servers
//	mcp-list       GET    /v1/mcp/servers
//	mcp-get        GET    /v1/mcp/servers/<id>
//	mcp-create     POST   /v1/mcp/servers
//	mcp-update     PUT    /v1/mcp/servers/<id>
//	mcp-delete     DELETE /v1/mcp/servers/<id>
//	mcp-test       POST   /v1/mcp/servers/<id>/test
//
//	# Approvals
//	approval-list  GET  /v1/approvals
//	approval-get   GET  /v1/approvals/<id>
//	approval-count GET  /v1/approvals/count
//	approve        POST /v1/approvals/<id>/approve
//	reject         POST /v1/approvals/<id>/reject
//
//	# Proposals & mentions
//	proposals      GET  /v1/aiops/mutating-proposals
//	mentions       GET  /v1/aiops/mentions/search
//
//	# Alert investigation
//	investigate    POST /v1/alerts/incidents/<id>/investigation
//	investigation  GET  /v1/alerts/incidents/<id>/investigation
//
//	# Reports
//	report-gen     POST /v1/reports
//	report-schedules GET /v1/report-schedules
//
// All commands auto-login first (unless --token is given) and print
// the JSON response body to stdout.  SSE events from chat-stream are
// printed as they arrive.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

// version is overwritten at build time via -ldflags "-X main.version=$(VERSION)".
var version = "dev"

// ---------------------------------------------------------------------
// global flags
// ---------------------------------------------------------------------

var (
	flagAddr   = flag.String("addr", "http://localhost:8080", "ongrid base URL")
	flagEmail  = flag.String("email", "admin@ongrid.local", "login email")
	flagPass   = flag.String("pass", "change-me-on-first-login", "login password")
	flagToken  = flag.String("token", "", "pre-existing bearer token (skip login)")
	flagJSON   = flag.Bool("json", false, "pretty-print JSON output")
	flagVer    = flag.Bool("v", false, "print version and exit")
)

func usage() {
	fmt.Fprintf(os.Stderr, "llm %s — ongrid LLM API test client\n\n", version)
	fmt.Fprintf(os.Stderr, "Usage: llm [flags] <command> [args]\n\n")
	fmt.Fprintf(os.Stderr, "Flags:\n")
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\nCommands:\n")
	for _, c := range cmds {
		fmt.Fprintf(os.Stderr, "  %-20s %s\n", c.name, c.short)
	}
	fmt.Fprintf(os.Stderr, "\nRun 'llm <command> -h' for command-specific help.\n")
}

// ---------------------------------------------------------------------
// command registry
// ---------------------------------------------------------------------

type cmd struct {
	name  string
	short string
	run   func(args []string)
}

var cmds []cmd

func reg(name, short string, run func(args []string)) {
	cmds = append(cmds, cmd{name, short, run})
}

func main() {
	flag.Usage = usage
	flag.Parse()
	if *flagVer {
		fmt.Fprintf(os.Stdout, "llm %s\n", version)
		return
	}
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(1)
	}
	for _, c := range cmds {
		if c.name == args[0] {
			c.run(args[1:])
			return
		}
	}
	fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
	flag.Usage()
	os.Exit(1)
}

// ---------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------

var log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// ensureToken logs in if needed and returns the bearer token.
func ensureToken() string {
	if *flagToken != "" {
		return *flagToken
	}
	tok, err := login(*flagAddr, *flagEmail, *flagPass)
	if err != nil {
		log.Error("login failed", slog.Any("err", err))
		os.Exit(1)
	}
	*flagToken = tok
	return tok
}

func login(base, email, pass string) (string, error) {
	body, _ := json.Marshal(map[string]string{"email": email, "password": pass})
	resp, err := http.Post(base+"/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("post /v1/auth/login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("login %d: %s", resp.StatusCode, b)
	}
	var lr struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return "", fmt.Errorf("decode login resp: %w", err)
	}
	log.Info("login ok", slog.String("email", email))
	return lr.AccessToken, nil
}

// doJSON sends a JSON request and returns the response body.
func doJSON(method, path string, payload interface{}) ([]byte, int) {
	tok := ensureToken()
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			fatal("marshal: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, *flagAddr+path, body)
	if err != nil {
		fatal("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fatal("request: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode
}

// doSSE does a streaming SSE request and prints events as they arrive.
func doSSE(method, path string, payload interface{}) {
	tok := ensureToken()
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			fatal("marshal: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, *flagAddr+path, body)
	if err != nil {
		fatal("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fatal("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		fatal("SSE %d: %s", resp.StatusCode, b)
	}
	scanner := bufio.NewScanner(resp.Body)
	var eventType string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			fmt.Printf("[%s] %s\n", eventType, data)
			if eventType == "done" || eventType == "error" {
				return
			}
		} else if line == "" {
			// end of event
		}
	}
}

// doUpload sends a multipart file upload.
func doUpload(path, filePath, title, docPath, tags string) ([]byte, int) {
	tok := ensureToken()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	f, err := os.Open(filePath)
	if err != nil {
		fatal("open %s: %v", filePath, err)
	}
	defer f.Close()
	fw, err := w.CreateFormFile("file", filePath)
	if err != nil {
		fatal("create form file: %v", err)
	}
	if _, err := io.Copy(fw, f); err != nil {
		fatal("copy file: %v", err)
	}
	if title != "" {
		_ = w.WriteField("title", title)
	}
	if docPath != "" {
		_ = w.WriteField("path", docPath)
	}
	if tags != "" {
		_ = w.WriteField("tags", tags)
	}
	w.Close()
	req, err := http.NewRequest("POST", *flagAddr+path, &buf)
	if err != nil {
		fatal("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fatal("request: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode
}

func printResp(body []byte, code int) {
	if *flagJSON {
		var buf bytes.Buffer
		if json.Indent(&buf, body, "", "  ") == nil {
			fmt.Printf("HTTP %d\n%s\n", code, buf.String())
		} else {
			fmt.Printf("HTTP %d\n%s\n", code, body)
		}
	} else {
		fmt.Printf("HTTP %d\n%s\n", code, body)
	}
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

// ---------------------------------------------------------------------
// subcommand flags
// ---------------------------------------------------------------------

func cmdFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: llm %s [flags] [args]\n\n", name)
		fs.PrintDefaults()
	}
	return fs
}

// ---------------------------------------------------------------------
// Commands: auth
// ---------------------------------------------------------------------

func init() {
	reg("login", "authenticate and print bearer token", func(args []string) {
		tok, err := login(*flagAddr, *flagEmail, *flagPass)
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println(tok)
	})
	reg("self", "GET /v1/self — current user info", func(args []string) {
		body, code := doJSON("GET", "/v1/self", nil)
		printResp(body, code)
	})
}

// ---------------------------------------------------------------------
// Commands: LLM configuration
// ---------------------------------------------------------------------

func init() {
	reg("llm-test", "POST /v1/integrations/llm/test — probe LLM provider", func(args []string) {
		fs := cmdFlags("llm-test")
		provider := fs.String("provider", "openai", "provider id")
		apiKey := fs.String("api-key", "", "API key")
		baseURL := fs.String("base-url", "", "custom base URL")
		model := fs.String("model", "", "default model")
		models := fs.String("models", "", "comma-separated model list")
		fs.Parse(args)
		var modelList []string
		if *models != "" {
			modelList = strings.Split(*models, ",")
		}
		payload := map[string]interface{}{
			"provider":      *provider,
			"api_key":       *apiKey,
			"base_url":      *baseURL,
			"default_model": *model,
			"models":        modelList,
		}
		body, code := doJSON("POST", "/v1/integrations/llm/test", payload)
		printResp(body, code)
	})

	reg("llm-save", "POST /v1/integrations/llm/validate-and-save — probe + save", func(args []string) {
		fs := cmdFlags("llm-save")
		provider := fs.String("provider", "openai", "provider id")
		apiKey := fs.String("api-key", "", "API key")
		baseURL := fs.String("base-url", "", "custom base URL")
		model := fs.String("model", "", "default model")
		models := fs.String("models", "", "comma-separated model list")
		fs.Parse(args)
		var modelList []string
		if *models != "" {
			modelList = strings.Split(*models, ",")
		}
		payload := map[string]interface{}{
			"provider":      *provider,
			"api_key":       *apiKey,
			"base_url":      *baseURL,
			"default_model": *model,
			"models":        modelList,
		}
		body, code := doJSON("POST", "/v1/integrations/llm/validate-and-save", payload)
		printResp(body, code)
	})

	reg("llm-invalidate", "POST /v1/integrations/llm/invalidate — flush router cache", func(args []string) {
		body, code := doJSON("POST", "/v1/integrations/llm/invalidate", nil)
		printResp(body, code)
	})

	reg("llm-settings", "GET /v1/system-settings?category=llm", func(args []string) {
		body, code := doJSON("GET", "/v1/system-settings?category=llm", nil)
		printResp(body, code)
	})

	reg("llm-setting", "PUT /v1/system-settings/llm/<key> — set single LLM setting", func(args []string) {
		fs := cmdFlags("llm-setting")
		fs.Parse(args)
		if fs.NArg() < 2 {
			fatal("usage: llm llm-setting <key> <value>")
		}
		key := fs.Arg(0)
		value := fs.Arg(1)
		payload := map[string]interface{}{"value": value}
		body, code := doJSON("PUT", "/v1/system-settings/llm/"+key, payload)
		printResp(body, code)
	})
}

// ---------------------------------------------------------------------
// Commands: model catalog & usage
// ---------------------------------------------------------------------

func init() {
	reg("models", "GET /v1/aiops/models — list available LLM providers/models", func(args []string) {
		body, code := doJSON("GET", "/v1/aiops/models", nil)
		printResp(body, code)
	})

	reg("usage", "GET /v1/usage/today — token usage today", func(args []string) {
		body, code := doJSON("GET", "/v1/usage/today", nil)
		printResp(body, code)
	})
}

// ---------------------------------------------------------------------
// Commands: chat sessions
// ---------------------------------------------------------------------

func init() {
	reg("session-create", "POST /v1/chat/sessions — create a new chat session", func(args []string) {
		fs := cmdFlags("session-create")
		title := fs.String("title", "llm-test", "session title")
		agent := fs.String("agent", "", "agent id (optional)")
		fs.Parse(args)
		payload := map[string]interface{}{
			"title": *title,
		}
		if *agent != "" {
			payload["agent_id"] = *agent
		}
		body, code := doJSON("POST", "/v1/chat/sessions", payload)
		printResp(body, code)
	})

	reg("session-list", "GET /v1/chat/sessions — list sessions", func(args []string) {
		fs := cmdFlags("session-list")
		limit := fs.Int("limit", 20, "page size")
		fs.Parse(args)
		body, code := doJSON("GET", fmt.Sprintf("/v1/chat/sessions?limit=%d", *limit), nil)
		printResp(body, code)
	})

	reg("session-rename", "PATCH /v1/chat/sessions/<id> — rename session", func(args []string) {
		fs := cmdFlags("session-rename")
		fs.Parse(args)
		if fs.NArg() < 2 {
			fatal("usage: llm session-rename <id> <title>")
		}
		id := fs.Arg(0)
		title := fs.Arg(1)
		body, code := doJSON("PATCH", "/v1/chat/sessions/"+id, map[string]string{"title": title})
		printResp(body, code)
	})

	reg("session-close", "DELETE /v1/chat/sessions/<id>", func(args []string) {
		fs := cmdFlags("session-close")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm session-close <id>")
		}
		body, code := doJSON("DELETE", "/v1/chat/sessions/"+fs.Arg(0), nil)
		printResp(body, code)
	})
}

// ---------------------------------------------------------------------
// Commands: messages
// ---------------------------------------------------------------------

func init() {
	reg("chat", "POST /v1/chat/sessions/<id>/messages — send message (sync)", func(args []string) {
		fs := cmdFlags("chat")
		provider := fs.String("provider", "", "override provider")
		model := fs.String("model", "", "override model")
		webSearch := fs.Bool("web-search", false, "enable web search")
		locale := fs.String("locale", "zh-CN", "locale")
		fs.Parse(args)
		if fs.NArg() < 2 {
			fatal("usage: llm chat <session-id> <content>")
		}
		id := fs.Arg(0)
		content := fs.Arg(1)
		payload := map[string]interface{}{
			"content":            content,
			"locale":             *locale,
			"web_search_enabled": *webSearch,
		}
		if *provider != "" {
			payload["provider"] = *provider
		}
		if *model != "" {
			payload["model"] = *model
		}
		body, code := doJSON("POST", "/v1/chat/sessions/"+id+"/messages", payload)
		printResp(body, code)
	})

	reg("chat-stream", "POST /v1/chat/sessions/<id>/messages/stream — SSE", func(args []string) {
		fs := cmdFlags("chat-stream")
		provider := fs.String("provider", "", "override provider")
		model := fs.String("model", "", "override model")
		webSearch := fs.Bool("web-search", false, "enable web search")
		locale := fs.String("locale", "zh-CN", "locale")
		fs.Parse(args)
		if fs.NArg() < 2 {
			fatal("usage: llm chat-stream <session-id> <content>")
		}
		id := fs.Arg(0)
		content := fs.Arg(1)
		payload := map[string]interface{}{
			"content":            content,
			"locale":             *locale,
			"web_search_enabled": *webSearch,
		}
		if *provider != "" {
			payload["provider"] = *provider
		}
		if *model != "" {
			payload["model"] = *model
		}
		doSSE("POST", "/v1/chat/sessions/"+id+"/messages/stream", payload)
	})

	reg("chat-stop", "POST /v1/chat/sessions/<id>/stop — interrupt in-flight turn", func(args []string) {
		fs := cmdFlags("chat-stop")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm chat-stop <session-id>")
		}
		body, code := doJSON("POST", "/v1/chat/sessions/"+fs.Arg(0)+"/stop", nil)
		printResp(body, code)
	})

	reg("messages", "GET /v1/chat/sessions/<id>/messages — list messages", func(args []string) {
		fs := cmdFlags("messages")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm messages <session-id>")
		}
		body, code := doJSON("GET", "/v1/chat/sessions/"+fs.Arg(0)+"/messages", nil)
		printResp(body, code)
	})
}

// ---------------------------------------------------------------------
// Commands: query translation
// ---------------------------------------------------------------------

func init() {
	reg("translate", "POST /v1/aiops/query-translate — NL → LogQL/TraceQL/PromQL", func(args []string) {
		fs := cmdFlags("translate")
		dialect := fs.String("dialect", "logql", "logql|traceql|promql")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm translate <prompt>")
		}
		payload := map[string]string{
			"dialect": *dialect,
			"prompt":  fs.Arg(0),
		}
		body, code := doJSON("POST", "/v1/aiops/query-translate", payload)
		printResp(body, code)
	})
}

// ---------------------------------------------------------------------
// Commands: agents
// ---------------------------------------------------------------------

func init() {
	reg("agent-list", "GET /v1/agents", func(args []string) {
		body, code := doJSON("GET", "/v1/agents", nil)
		printResp(body, code)
	})

	reg("agent-get", "GET /v1/agents/<name>", func(args []string) {
		fs := cmdFlags("agent-get")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm agent-get <name>")
		}
		body, code := doJSON("GET", "/v1/agents/"+fs.Arg(0), nil)
		printResp(body, code)
	})

	reg("agent-create", "POST /v1/agents/custom", func(args []string) {
		fs := cmdFlags("agent-create")
		name := fs.String("name", "", "agent name")
		desc := fs.String("desc", "", "description")
		prompt := fs.String("prompt", "", "system prompt")
		fs.Parse(args)
		if *name == "" {
			fatal("--name required")
		}
		payload := map[string]string{
			"name":         *name,
			"description":  *desc,
			"system_prompt": *prompt,
		}
		body, code := doJSON("POST", "/v1/agents/custom", payload)
		printResp(body, code)
	})

	reg("agent-update", "PATCH /v1/agents/custom/<name>", func(args []string) {
		fs := cmdFlags("agent-update")
		desc := fs.String("desc", "", "description")
		prompt := fs.String("prompt", "", "system prompt")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm agent-update <name>")
		}
		payload := map[string]string{}
		if *desc != "" {
			payload["description"] = *desc
		}
		if *prompt != "" {
			payload["system_prompt"] = *prompt
		}
		body, code := doJSON("PATCH", "/v1/agents/custom/"+fs.Arg(0), payload)
		printResp(body, code)
	})

	reg("agent-delete", "DELETE /v1/agents/custom/<name>", func(args []string) {
		fs := cmdFlags("agent-delete")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm agent-delete <name>")
		}
		body, code := doJSON("DELETE", "/v1/agents/custom/"+fs.Arg(0), nil)
		printResp(body, code)
	})
}

// ---------------------------------------------------------------------
// Commands: skills
// ---------------------------------------------------------------------

func init() {
	reg("skill-list", "GET /v1/skills", func(args []string) {
		fs := cmdFlags("skill-list")
		category := fs.String("category", "", "filter by category")
		fs.Parse(args)
		q := ""
		if *category != "" {
			q = "?category=" + *category
		}
		body, code := doJSON("GET", "/v1/skills"+q, nil)
		printResp(body, code)
	})

	reg("skill-get", "GET /v1/skills/<key>", func(args []string) {
		fs := cmdFlags("skill-get")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm skill-get <key>")
		}
		body, code := doJSON("GET", "/v1/skills/"+fs.Arg(0), nil)
		printResp(body, code)
	})

	reg("skill-exec", "POST /v1/skills/<key>/execute", func(args []string) {
		fs := cmdFlags("skill-exec")
		edgeID := fs.Uint64("edge-id", 0, "edge id (required for scope=host)")
		params := fs.String("params", "{}", "JSON params")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm skill-exec <key>")
		}
		payload := map[string]interface{}{}
		if *edgeID > 0 {
			payload["edge_id"] = *edgeID
		}
		var p json.RawMessage
		if json.Unmarshal([]byte(*params), &p) == nil {
			payload["params"] = p
		}
		body, code := doJSON("POST", "/v1/skills/"+fs.Arg(0)+"/execute", payload)
		printResp(body, code)
	})
}

// ---------------------------------------------------------------------
// Commands: knowledge / RAG
// ---------------------------------------------------------------------

func init() {
	reg("doc-list", "GET /v1/knowledge/docs", func(args []string) {
		fs := cmdFlags("doc-list")
		limit := fs.Int("limit", 20, "page size")
		path := fs.String("path", "", "filter by path")
		tag := fs.String("tag", "", "filter by tag")
		fs.Parse(args)
		q := fmt.Sprintf("?limit=%d", *limit)
		if *path != "" {
			q += "&path=" + *path
		}
		if *tag != "" {
			q += "&tag=" + *tag
		}
		body, code := doJSON("GET", "/v1/knowledge/docs"+q, nil)
		printResp(body, code)
	})

	reg("doc-get", "GET /v1/knowledge/docs/<id>", func(args []string) {
		fs := cmdFlags("doc-get")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm doc-get <id>")
		}
		body, code := doJSON("GET", "/v1/knowledge/docs/"+fs.Arg(0), nil)
		printResp(body, code)
	})

	reg("doc-create", "POST /v1/knowledge/docs", func(args []string) {
		fs := cmdFlags("doc-create")
		title := fs.String("title", "", "doc title")
		content := fs.String("content", "", "doc content")
		docPath := fs.String("path", "", "doc path")
		tags := fs.String("tags", "", "comma-separated tags")
		fs.Parse(args)
		if *title == "" {
			fatal("--title required")
		}
		payload := map[string]string{
			"title":   *title,
			"content": *content,
		}
		if *docPath != "" {
			payload["path"] = *docPath
		}
		if *tags != "" {
			payload["tags"] = *tags
		}
		body, code := doJSON("POST", "/v1/knowledge/docs", payload)
		printResp(body, code)
	})

	reg("doc-update", "PATCH /v1/knowledge/docs/<id>", func(args []string) {
		fs := cmdFlags("doc-update")
		title := fs.String("title", "", "doc title")
		content := fs.String("content", "", "doc content")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm doc-update <id>")
		}
		payload := map[string]string{}
		if *title != "" {
			payload["title"] = *title
		}
		if *content != "" {
			payload["content"] = *content
		}
		body, code := doJSON("PATCH", "/v1/knowledge/docs/"+fs.Arg(0), payload)
		printResp(body, code)
	})

	reg("doc-delete", "DELETE /v1/knowledge/docs/<id>", func(args []string) {
		fs := cmdFlags("doc-delete")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm doc-delete <id>")
		}
		body, code := doJSON("DELETE", "/v1/knowledge/docs/"+fs.Arg(0), nil)
		printResp(body, code)
	})

	reg("doc-upload", "POST /v1/knowledge/upload — multipart file upload", func(args []string) {
		fs := cmdFlags("doc-upload")
		title := fs.String("title", "", "doc title")
		docPath := fs.String("path", "", "doc path")
		tags := fs.String("tags", "", "comma-separated tags")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm doc-upload <file-path>")
		}
		body, code := doUpload("/v1/knowledge/upload", fs.Arg(0), *title, *docPath, *tags)
		printResp(body, code)
	})

	reg("doc-move", "PATCH /v1/knowledge/docs/<id>/move", func(args []string) {
		fs := cmdFlags("doc-move")
		fs.Parse(args)
		if fs.NArg() < 2 {
			fatal("usage: llm doc-move <id> <new-path>")
		}
		payload := map[string]string{"path": fs.Arg(1)}
		body, code := doJSON("PATCH", "/v1/knowledge/docs/"+fs.Arg(0)+"/move", payload)
		printResp(body, code)
	})

	reg("kn-search", "GET /v1/knowledge/search?q=...", func(args []string) {
		fs := cmdFlags("kn-search")
		limit := fs.Int("limit", 5, "max results")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm kn-search <query>")
		}
		q := fs.Arg(0)
		body, code := doJSON("GET", fmt.Sprintf("/v1/knowledge/search?q=%s&limit=%d", q, *limit), nil)
		printResp(body, code)
	})

	reg("kn-paths", "GET /v1/knowledge/paths", func(args []string) {
		body, code := doJSON("GET", "/v1/knowledge/paths", nil)
		printResp(body, code)
	})

	reg("repo-list", "GET /v1/knowledge/repos", func(args []string) {
		body, code := doJSON("GET", "/v1/knowledge/repos", nil)
		printResp(body, code)
	})

	reg("repo-create", "POST /v1/knowledge/repos", func(args []string) {
		fs := cmdFlags("repo-create")
		branch := fs.String("branch", "main", "git branch")
		desc := fs.String("desc", "", "description")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm repo-create <git-url>")
		}
		payload := map[string]string{
			"url":         fs.Arg(0),
			"branch":      *branch,
			"description": *desc,
		}
		body, code := doJSON("POST", "/v1/knowledge/repos", payload)
		printResp(body, code)
	})

	reg("repo-sync", "POST /v1/knowledge/repos/<id>/sync", func(args []string) {
		fs := cmdFlags("repo-sync")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm repo-sync <id>")
		}
		body, code := doJSON("POST", "/v1/knowledge/repos/"+fs.Arg(0)+"/sync", nil)
		printResp(body, code)
	})

	reg("repo-delete", "DELETE /v1/knowledge/repos/<id>", func(args []string) {
		fs := cmdFlags("repo-delete")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm repo-delete <id>")
		}
		body, code := doJSON("DELETE", "/v1/knowledge/repos/"+fs.Arg(0), nil)
		printResp(body, code)
	})

	reg("vault-sync", "POST /v1/knowledge/vault/sync", func(args []string) {
		body, code := doJSON("POST", "/v1/knowledge/vault/sync", nil)
		printResp(body, code)
	})
}

// ---------------------------------------------------------------------
// Commands: MCP servers
// ---------------------------------------------------------------------

func init() {
	reg("mcp-list", "GET /v1/mcp/servers", func(args []string) {
		body, code := doJSON("GET", "/v1/mcp/servers", nil)
		printResp(body, code)
	})

	reg("mcp-get", "GET /v1/mcp/servers/<id>", func(args []string) {
		fs := cmdFlags("mcp-get")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm mcp-get <id>")
		}
		body, code := doJSON("GET", "/v1/mcp/servers/"+fs.Arg(0), nil)
		printResp(body, code)
	})

	reg("mcp-create", "POST /v1/mcp/servers", func(args []string) {
		fs := cmdFlags("mcp-create")
		name := fs.String("name", "", "server name")
		transport := fs.String("transport", "http", "stdio|http|sse")
		endpoint := fs.String("endpoint", "", "server endpoint URL")
		fs.Parse(args)
		if *name == "" {
			fatal("--name required")
		}
		payload := map[string]string{
			"name":      *name,
			"transport": *transport,
			"endpoint":  *endpoint,
		}
		body, code := doJSON("POST", "/v1/mcp/servers", payload)
		printResp(body, code)
	})

	reg("mcp-update", "PUT /v1/mcp/servers/<id>", func(args []string) {
		fs := cmdFlags("mcp-update")
		name := fs.String("name", "", "server name")
		endpoint := fs.String("endpoint", "", "server endpoint URL")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm mcp-update <id>")
		}
		payload := map[string]string{}
		if *name != "" {
			payload["name"] = *name
		}
		if *endpoint != "" {
			payload["endpoint"] = *endpoint
		}
		body, code := doJSON("PUT", "/v1/mcp/servers/"+fs.Arg(0), payload)
		printResp(body, code)
	})

	reg("mcp-delete", "DELETE /v1/mcp/servers/<id>", func(args []string) {
		fs := cmdFlags("mcp-delete")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm mcp-delete <id>")
		}
		body, code := doJSON("DELETE", "/v1/mcp/servers/"+fs.Arg(0), nil)
		printResp(body, code)
	})

	reg("mcp-test", "POST /v1/mcp/servers/<id>/test — list tools", func(args []string) {
		fs := cmdFlags("mcp-test")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm mcp-test <id>")
		}
		body, code := doJSON("POST", "/v1/mcp/servers/"+fs.Arg(0)+"/test", nil)
		printResp(body, code)
	})
}

// ---------------------------------------------------------------------
// Commands: approvals
// ---------------------------------------------------------------------

func init() {
	reg("approval-list", "GET /v1/approvals", func(args []string) {
		body, code := doJSON("GET", "/v1/approvals", nil)
		printResp(body, code)
	})

	reg("approval-count", "GET /v1/approvals/count", func(args []string) {
		body, code := doJSON("GET", "/v1/approvals/count", nil)
		printResp(body, code)
	})

	reg("approval-get", "GET /v1/approvals/<id>", func(args []string) {
		fs := cmdFlags("approval-get")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm approval-get <id>")
		}
		body, code := doJSON("GET", "/v1/approvals/"+fs.Arg(0), nil)
		printResp(body, code)
	})

	reg("approve", "POST /v1/approvals/<id>/approve", func(args []string) {
		fs := cmdFlags("approve")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm approve <id>")
		}
		body, code := doJSON("POST", "/v1/approvals/"+fs.Arg(0)+"/approve", nil)
		printResp(body, code)
	})

	reg("reject", "POST /v1/approvals/<id>/reject", func(args []string) {
		fs := cmdFlags("reject")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm reject <id>")
		}
		body, code := doJSON("POST", "/v1/approvals/"+fs.Arg(0)+"/reject", nil)
		printResp(body, code)
	})
}

// ---------------------------------------------------------------------
// Commands: proposals & mentions
// ---------------------------------------------------------------------

func init() {
	reg("proposals", "GET /v1/aiops/mutating-proposals", func(args []string) {
		fs := cmdFlags("proposals")
		limit := fs.Int("limit", 20, "page size")
		toolName := fs.String("tool", "", "filter by tool name")
		fs.Parse(args)
		q := fmt.Sprintf("?limit=%d", *limit)
		if *toolName != "" {
			q += "&tool_name=" + *toolName
		}
		body, code := doJSON("GET", "/v1/aiops/mutating-proposals"+q, nil)
		printResp(body, code)
	})

	reg("mentions", "GET /v1/aiops/mentions/search?q=...", func(args []string) {
		fs := cmdFlags("mentions")
		mtype := fs.String("type", "", "edge|incident|alert")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm mentions <query>")
		}
		q := "?q=" + fs.Arg(0)
		if *mtype != "" {
			q += "&type=" + *mtype
		}
		body, code := doJSON("GET", "/v1/aiops/mentions/search"+q, nil)
		printResp(body, code)
	})
}

// ---------------------------------------------------------------------
// Commands: alert investigation
// ---------------------------------------------------------------------

func init() {
	reg("investigate", "POST /v1/alerts/incidents/<id>/investigation — trigger RCA", func(args []string) {
		fs := cmdFlags("investigate")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm investigate <incident-id>")
		}
		body, code := doJSON("POST", "/v1/alerts/incidents/"+fs.Arg(0)+"/investigation", nil)
		printResp(body, code)
	})

	reg("investigation", "GET /v1/alerts/incidents/<id>/investigation — read RCA", func(args []string) {
		fs := cmdFlags("investigation")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm investigation <incident-id>")
		}
		body, code := doJSON("GET", "/v1/alerts/incidents/"+fs.Arg(0)+"/investigation", nil)
		printResp(body, code)
	})
}

// ---------------------------------------------------------------------
// Commands: reports
// ---------------------------------------------------------------------

func init() {
	reg("report-gen", "POST /v1/reports — generate a report", func(args []string) {
		fs := cmdFlags("report-gen")
		kind := fs.String("kind", "daily", "daily|weekly|monthly")
		tz := fs.String("tz", "Asia/Shanghai", "timezone")
		fs.Parse(args)
		payload := map[string]string{
			"kind":     *kind,
			"timezone": *tz,
		}
		body, code := doJSON("POST", "/v1/reports", payload)
		printResp(body, code)
	})

	reg("report-schedules", "GET /v1/report-schedules", func(args []string) {
		body, code := doJSON("GET", "/v1/report-schedules", nil)
		printResp(body, code)
	})
}

// ---------------------------------------------------------------------
// Commands: convenience — one-shot chat
// ---------------------------------------------------------------------

func init() {
	reg("ask", "convenience: create session → chat → print reply → close session", func(args []string) {
		fs := cmdFlags("ask")
		provider := fs.String("provider", "", "override provider")
		model := fs.String("model", "", "override model")
		webSearch := fs.Bool("web-search", false, "enable web search")
		locale := fs.String("locale", "zh-CN", "locale")
		keep := fs.Bool("keep", false, "keep session open (don't delete)")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm ask <question>")
		}
		content := fs.Arg(0)

		// 1. create session
		sessPayload := map[string]interface{}{"title": "llm-ask"}
		sessBody, code := doJSON("POST", "/v1/chat/sessions", sessPayload)
		if code != 201 {
			fatal("create session %d: %s", code, sessBody)
		}
		var sess struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(sessBody, &sess); err != nil {
			fatal("decode session: %v", err)
		}
		log.Info("session created", slog.String("id", sess.ID))

		// 2. send message
		msgPayload := map[string]interface{}{
			"content":            content,
			"locale":             *locale,
			"web_search_enabled": *webSearch,
		}
		if *provider != "" {
			msgPayload["provider"] = *provider
		}
		if *model != "" {
			msgPayload["model"] = *model
		}
		msgBody, code := doJSON("POST", "/v1/chat/sessions/"+sess.ID+"/messages", msgPayload)
		if code != 200 {
			log.Warn("chat failed", slog.Int("code", code), slog.String("body", string(msgBody)))
		} else {
			// extract assistant content
			var resp struct {
				AssistantMessage struct {
					Content string `json:"content"`
				} `json:"assistant_message"`
				Usage struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
					TotalTokens      int `json:"total_tokens"`
				} `json:"usage"`
				Iterations int `json:"iterations"`
			}
			if json.Unmarshal(msgBody, &resp) == nil {
				fmt.Println(resp.AssistantMessage.Content)
				log.Info("usage",
					slog.Int("prompt", resp.Usage.PromptTokens),
					slog.Int("completion", resp.Usage.CompletionTokens),
					slog.Int("total", resp.Usage.TotalTokens),
					slog.Int("iterations", resp.Iterations),
				)
			} else {
				fmt.Println(string(msgBody))
			}
		}

		// 3. close session
		if !*keep {
			doJSON("DELETE", "/v1/chat/sessions/"+sess.ID, nil)
			log.Info("session closed", slog.String("id", sess.ID))
		} else {
			log.Info("session kept", slog.String("id", sess.ID))
		}
	})

	reg("ask-stream", "convenience: create session → chat-stream → close session", func(args []string) {
		fs := cmdFlags("ask-stream")
		provider := fs.String("provider", "", "override provider")
		model := fs.String("model", "", "override model")
		webSearch := fs.Bool("web-search", false, "enable web search")
		locale := fs.String("locale", "zh-CN", "locale")
		keep := fs.Bool("keep", false, "keep session open")
		fs.Parse(args)
		if fs.NArg() < 1 {
			fatal("usage: llm ask-stream <question>")
		}
		content := fs.Arg(0)

		// 1. create session
		sessPayload := map[string]interface{}{"title": "llm-ask-stream"}
		sessBody, code := doJSON("POST", "/v1/chat/sessions", sessPayload)
		if code != 201 {
			fatal("create session %d: %s", code, sessBody)
		}
		var sess struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(sessBody, &sess); err != nil {
			fatal("decode session: %v", err)
		}
		log.Info("session created", slog.String("id", sess.ID))

		// 2. stream
		msgPayload := map[string]interface{}{
			"content":            content,
			"locale":             *locale,
			"web_search_enabled": *webSearch,
		}
		if *provider != "" {
			msgPayload["provider"] = *provider
		}
		if *model != "" {
			msgPayload["model"] = *model
		}
		doSSE("POST", "/v1/chat/sessions/"+sess.ID+"/messages/stream", msgPayload)

		// 3. close session
		if !*keep {
			doJSON("DELETE", "/v1/chat/sessions/"+sess.ID, nil)
			log.Info("session closed", slog.String("id", sess.ID))
		} else {
			log.Info("session kept", slog.String("id", sess.ID))
		}
	})
}

// ---------------------------------------------------------------------
// Commands: e2e smoke test
// ---------------------------------------------------------------------

func init() {
	reg("smoke", "run end-to-end smoke test covering all LLM features", func(args []string) {
		start := time.Now()
		passed := 0
		failed := 0

		check := func(name string, fn func() error) {
			if err := fn(); err != nil {
				fmt.Printf("  FAIL  %s: %v\n", name, err)
				failed++
			} else {
				fmt.Printf("  PASS  %s\n", name)
				passed++
			}
		}

		fmt.Println("=== ongrid LLM API smoke test ===")
		fmt.Println()

		// 1. Auth
		check("login", func() error {
			_, err := login(*flagAddr, *flagEmail, *flagPass)
			return err
		})
		check("self", func() error {
			body, code := doJSON("GET", "/v1/self", nil)
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})

		// 2. LLM config
		check("llm-settings", func() error {
			body, code := doJSON("GET", "/v1/system-settings?category=llm", nil)
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})
		check("models", func() error {
			body, code := doJSON("GET", "/v1/aiops/models", nil)
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})
		check("usage", func() error {
			body, code := doJSON("GET", "/v1/usage/today", nil)
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})

		// 3. Chat session + message
		var sessionID string
		check("session-create", func() error {
			body, code := doJSON("POST", "/v1/chat/sessions", map[string]string{"title": "smoke-test"})
			if code != 201 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			var s struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(body, &s); err != nil {
				return fmt.Errorf("decode: %w", err)
			}
			sessionID = s.ID
			return nil
		})
		check("session-list", func() error {
			body, code := doJSON("GET", "/v1/chat/sessions?limit=5", nil)
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})
		if sessionID != "" {
			check("chat", func() error {
				body, code := doJSON("POST", "/v1/chat/sessions/"+sessionID+"/messages",
					map[string]interface{}{"content": "ping", "locale": "zh-CN"})
				if code != 200 {
					return fmt.Errorf("HTTP %d: %s", code, body)
				}
				return nil
			})
			check("messages", func() error {
				body, code := doJSON("GET", "/v1/chat/sessions/"+sessionID+"/messages", nil)
				if code != 200 {
					return fmt.Errorf("HTTP %d: %s", code, body)
				}
				return nil
			})
			check("session-close", func() error {
				body, code := doJSON("DELETE", "/v1/chat/sessions/"+sessionID, nil)
				if code != 204 && code != 200 {
					return fmt.Errorf("HTTP %d: %s", code, body)
				}
				return nil
			})
		}

		// 4. Query translate
		check("translate", func() error {
			body, code := doJSON("POST", "/v1/aiops/query-translate",
				map[string]string{"dialect": "logql", "prompt": "error logs"})
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})

		// 5. Agents
		check("agent-list", func() error {
			body, code := doJSON("GET", "/v1/agents", nil)
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})

		// 6. Skills
		check("skill-list", func() error {
			body, code := doJSON("GET", "/v1/skills", nil)
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})

		// 7. Knowledge
		check("doc-list", func() error {
			body, code := doJSON("GET", "/v1/knowledge/docs?limit=5", nil)
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})
		check("kn-search", func() error {
			body, code := doJSON("GET", "/v1/knowledge/search?q=test&limit=3", nil)
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})
		check("kn-paths", func() error {
			body, code := doJSON("GET", "/v1/knowledge/paths", nil)
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})

		// 8. MCP
		check("mcp-list", func() error {
			body, code := doJSON("GET", "/v1/mcp/servers", nil)
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})

		// 9. Approvals
		check("approval-list", func() error {
			body, code := doJSON("GET", "/v1/approvals", nil)
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})
		check("approval-count", func() error {
			body, code := doJSON("GET", "/v1/approvals/count", nil)
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})

		// 10. Proposals & mentions
		check("proposals", func() error {
			body, code := doJSON("GET", "/v1/aiops/mutating-proposals?limit=5", nil)
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})

		// 11. LLM invalidate
		check("llm-invalidate", func() error {
			body, code := doJSON("POST", "/v1/integrations/llm/invalidate", nil)
			if code != 200 {
				return fmt.Errorf("HTTP %d: %s", code, body)
			}
			return nil
		})

		fmt.Println()
		fmt.Printf("=== smoke: %d passed, %d failed, %.1fs ===\n", passed, failed, time.Since(start).Seconds())
		if failed > 0 {
			os.Exit(1)
		}
	})
}
