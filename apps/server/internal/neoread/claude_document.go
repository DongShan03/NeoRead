package neoread

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"neoread.local/server/internal/runtimeenv"
)

// Unlike --restricted, this preserves settings.env gateways and model defaults.
// The CLI ignores project/local settings; explicit settings disable hooks,
// auto-memory and CLAUDE.md discovery. No built-in filesystem/command tools are
// exposed. The system/init inventory must then match before any draft escapes.
func claudeDocumentArgs(config string, web bool) []string {
	tools := ""
	allowed := []string{}
	for _, definition := range documentToolDefinitions {
		allowed = append(allowed, "mcp__neoread_document__"+definition.Name)
	}
	if web {
		tools = "WebSearch,WebFetch"
		allowed = append(allowed, "WebSearch", "WebFetch")
	}
	return []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--tools", tools, "--strict-mcp-config", "--mcp-config", config,
		"--allowedTools", strings.Join(allowed, ","), "--permission-mode", "dontAsk", "--permission-prompts", "none",
		"--setting-sources", "user", "--settings", `{"disableAllHooks":true,"autoMemoryEnabled":false,"claudeMdExcludes":["**"]}`,
		"--disable-slash-commands", "--no-session-persistence",
		"--system-prompt", webInstructions(documentReadingInstructions, web)}
}

func claudeDocumentInventory(raw json.RawMessage, web bool) string {
	var init struct {
		Tools []string `json:"tools"`
		MCP   []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"mcp_servers"`
	}
	if json.Unmarshal(raw, &init) != nil {
		return "missing_tools"
	}
	expected := map[string]bool{}
	for _, definition := range documentToolDefinitions {
		expected["mcp__neoread_document__"+definition.Name] = true
	}
	if web {
		expected["WebSearch"], expected["WebFetch"] = true, true
	}
	seen := map[string]bool{}
	for _, tool := range init.Tools {
		if !expected[tool] || seen[tool] {
			return "extra_tools"
		}
		seen[tool] = true
	}
	for _, server := range init.MCP {
		if server.Name != "neoread_document" {
			return "extra_tools"
		}
	}
	if len(seen) != len(expected) || len(init.MCP) != 1 || init.MCP[0].Status != "connected" {
		return "missing_tools"
	}
	return "passed"
}

func streamClaudeDocument(ctx context.Context, work, model, url, token string, in AIInput, emit func(ProviderEvent) error) (GenerateResult, error) {
	config, err := writeDocumentMCPConfig(work, url, token)
	if err != nil {
		return GenerateResult{}, err
	}
	args := claudeDocumentArgs(config, in.Chat.WebSearch)
	if model != "" {
		args = append(args, "--model", model)
	}
	if in.Effort != "" {
		args = append(args, "--effort", in.Effort)
	}
	var input io.Reader = strings.NewReader(in.Prompt)
	if len(in.images()) > 0 {
		args = append(args, "--input-format", "stream-json")
		parts := []any{}
		for _, image := range in.images() {
			parts = append(parts, map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(image)}})
		}
		parts = append(parts, map[string]string{"type": "text", "text": in.Prompt})
		input = bytes.NewBufferString(jsonText(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": parts}}) + "\n")
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := runtimeenv.CommandContext(child, "claude", args...)
	cmd.Dir, cmd.Stdin, cmd.Stderr = work, input, io.Discard
	cmd.WaitDelay = 400 * time.Millisecond
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return GenerateResult{}, err
	}
	if err = cmd.Start(); err != nil {
		return GenerateResult{}, generationError(ErrorConfiguration, "无法启动 Claude Code")
	}
	stop := context.AfterFunc(child, func() { time.AfterFunc(450*time.Millisecond, func() { _ = stdout.Close() }) })
	defer stop()
	state := claudeDocumentStream{chat: in.Chat, draft: cliDocumentDraft{emit: emit}}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), documentCLIFrameBytes)
	for scanner.Scan() {
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
		if state.completed {
			continue
		}
		reportClaudeMetrics(scanner.Bytes(), []func([]ModelTokens){func(metrics []ModelTokens) {
			if emit != nil && err == nil {
				err = emit(ProviderEvent{Metrics: metrics})
			}
		}})
		if err == nil {
			err = state.consume(scanner.Bytes())
		}
		if err != nil {
			break
		}
	}
	if err == nil {
		err = scanner.Err()
	}
	if err != nil {
		cancel()
	}
	exit := cmd.Wait()
	if err != nil {
		return GenerateResult{}, err
	}
	if ctx.Err() != nil {
		return GenerateResult{}, ctx.Err()
	}
	if exit != nil || !state.completed || !state.initialized || state.stop != "end_turn" {
		return GenerateResult{}, generationError(ErrorProtocol, "Claude 未完成文档回答")
	}
	return state.draft.finish(ctx)
}

type claudeDocumentStream struct {
	chat        *chatInput
	draft       cliDocumentDraft
	initialized bool
	completed   bool
	stop        string
}

func (s *claudeDocumentStream) consume(raw []byte) error {
	var event struct {
		Type       string          `json:"type"`
		Subtype    string          `json:"subtype"`
		Parent     string          `json:"parent_tool_use_id"`
		IsError    bool            `json:"is_error"`
		Result     string          `json:"result"`
		ToolResult json.RawMessage `json:"tool_use_result"`
		Event      struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
				Stop string `json:"stop_reason"`
			} `json:"delta"`
			Block struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content_block"`
		} `json:"event"`
		Message struct {
			Content []claudeToolBlock `json:"content"`
		} `json:"message"`
	}
	if !utf8.Valid(raw) || json.Unmarshal(raw, &event) != nil {
		return generationError(ErrorProtocol, "Claude 文档流格式无效")
	}
	if event.Parent != "" {
		return generationError(ErrorCapability, "Claude 返回了未授权的子任务")
	}
	if event.IsError || event.Type == "error" || event.Type == "result" && event.Subtype != "success" {
		return generationError(ErrorUpstream, "Claude 文档请求失败")
	}
	if event.Type == "system" && event.Subtype == "init" {
		status := claudeDocumentInventory(raw, s.chat.WebSearch)
		level := slog.LevelInfo
		if status != "passed" {
			level = slog.LevelError
		}
		s.chat.Agent.server.diagnosticEvent(level, "chat.document_tools", "provider", "claude", "status", status)
		if s.initialized || status != "passed" {
			return generationError(ErrorCapability, "Claude 文档工具范围与本轮权限不符")
		}
		s.initialized = true
		return nil
	}
	if !s.initialized && (event.Type == "stream_event" || event.Type == "assistant" || event.Type == "user" || event.Type == "result") {
		return generationError(ErrorCapability, "Claude 未确认文档工具清单")
	}
	switch event.Type {
	case "stream_event":
		switch event.Event.Type {
		case "message_start":
			s.stop = ""
		case "content_block_start":
			if event.Event.Block.Type == "text" {
				return s.draft.add(event.Event.Block.Text)
			}
		case "content_block_delta":
			if event.Event.Delta.Type == "text_delta" {
				return s.draft.add(event.Event.Delta.Text)
			}
		case "message_delta":
			if event.Event.Delta.Stop != "" {
				s.stop = event.Event.Delta.Stop
				if s.stop == "tool_use" {
					return s.draft.reset()
				}
			}
		}
	case "assistant", "user":
		for _, block := range event.Message.Content {
			if block.Type == "tool_result" && len(event.ToolResult) > 0 {
				block.Content = event.ToolResult
			}
			if err := s.toolBlock(block); err != nil {
				return err
			}
		}
	case "result":
		if s.stop != "end_turn" || event.Result != s.draft.text {
			return generationError(ErrorProtocol, "Claude 最终回答与流式内容不一致")
		}
		s.completed = true
	}
	return nil
}

type claudeToolBlock struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	CallID  string          `json:"tool_use_id"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

func (s *claudeDocumentStream) toolBlock(block claudeToolBlock) error {
	if block.Type == "tool_use" {
		name := strings.TrimPrefix(block.Name, "mcp__neoread_document__")
		if !(strings.HasPrefix(block.Name, "mcp__neoread_document__") && documentToolName(name)) && !(s.chat.WebSearch && (block.Name == "WebSearch" || block.Name == "WebFetch")) {
			return generationError(ErrorCapability, "Claude 调用了未授权工具")
		}
		v := ToolActivity{ID: block.ID, Kind: "tool", Tool: "neoread_document/" + name, Title: "调用工具", Target: toolRawArguments(block.Input), Status: "running"}
		if block.Name == "WebSearch" || block.Name == "WebFetch" {
			v.Kind, v.Tool, v.Title = "search", "web_search", "搜索网页"
			if block.Name == "WebFetch" {
				v.Kind, v.Tool, v.Title, v.URL = "web", "web_fetch", "读取网页", v.Target
			}
		}
		return s.chat.recordTool(v)
	}
	if block.Type == "tool_result" {
		for _, v := range s.chat.toolSnapshot() {
			if v.ID != block.CallID {
				continue
			}
			v.Status = "completed"
			if block.IsError {
				v.Status = "failed"
			} else if v.Tool == "web_search" || v.Tool == "web_fetch" {
				s.chat.claudeWebSources(v, block.Content)
			}
			return s.chat.recordTool(v)
		}
	}
	return nil
}

func (c *chatInput) claudeWebSources(tool ToolActivity, raw json.RawMessage) {
	// Native tool results are structured inconsistently across CLI versions.
	// Preserve only returned URLs/title/text, never infer URLs from citation IDs.
	var visit func(json.RawMessage)
	visit = func(raw json.RawMessage) {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) == nil && object != nil {
			var source WebSource
			_ = json.Unmarshal(object["url"], &source.URL)
			_ = json.Unmarshal(object["title"], &source.Title)
			_ = json.Unmarshal(object["text"], &source.Text)
			if source.URL != "" {
				source.Kind = "search"
				if tool.Tool == "web_fetch" {
					source.Kind = "page"
				}
				c.addWebSource(source)
			}
			for _, value := range object {
				visit(value)
			}
			return
		}
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) == nil {
			for _, value := range list {
				visit(value)
			}
			return
		}
		var text string
		if json.Unmarshal(raw, &text) == nil {
			if json.Valid([]byte(text)) {
				visit(json.RawMessage(text))
			} else if _, links, found := strings.Cut(text, "Links: "); found {
				var items json.RawMessage
				if json.NewDecoder(strings.NewReader(links)).Decode(&items) == nil {
					visit(items)
				}
			}
		}
	}
	visit(raw)
	if tool.Tool == "web_fetch" && tool.URL != "" {
		c.addWebSource(WebSource{URL: tool.URL, Title: tool.URL, Kind: "page"})
	}
}
