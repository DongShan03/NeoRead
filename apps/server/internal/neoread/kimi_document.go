package neoread

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"neoread.local/server/internal/runtimeenv"
)

type documentMCPTransport struct{ token string }

func (t documentMCPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(request)
}

func documentMCPHTTPClient(token string) *http.Client {
	return &http.Client{Transport: documentMCPTransport{token}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func checkBoundDocumentMCP(ctx context.Context, url, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "neoread-preflight", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: documentMCPHTTPClient(token), DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		return err
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, tool := range tools.Tools {
		if !documentToolName(tool.Name) || seen[tool.Name] {
			return generationError(ErrorCapability, "文档 MCP 存在额外工具")
		}
		seen[tool.Name] = true
	}
	if len(seen) != len(documentToolDefinitions) || tools.NextCursor != "" {
		return generationError(ErrorCapability, "文档 MCP 缺少工具")
	}
	return nil
}

var errKimiSessionRejected = errors.New("Kimi session rejected")

func streamKimiDocument(ctx context.Context, work, model, url, token string, in AIInput, emit func(ProviderEvent) error) (GenerateResult, error) {
	profile := filepath.Join(work, "neoread.md")
	// Never advertise terminal/filesystem capabilities or built-in tools. Kimi
	// ACP has no remote inventory API; runtime tool events are checked below.
	if err := os.WriteFile(profile, []byte("---\nname: reader\ndescription: NeoRead document assistant\ntools: []\nsubagents: []\n---\n"+webInstructions(documentReadingRules, false)+"\n未注册文档工具时，仅依据应用提供的原文摘录回答，不虚构工具调用。\n"), 0600); err != nil {
		return GenerateResult{}, err
	}
	args := []string{"--agent-file", profile, "--skills-dir", work}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "acp")
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := runtimeenv.CommandContext(context.WithoutCancel(child), "kimi", args...)
	cmd.Dir, cmd.Stderr, cmd.WaitDelay = work, io.Discard, 3*time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return GenerateResult{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return GenerateResult{}, err
	}
	if err = cmd.Start(); err != nil {
		return GenerateResult{}, generationError(ErrorConfiguration, "无法启动 Kimi Code")
	}
	life := newRPCLifecycle(child, cmd, stdin, stdout)
	defer func() { life.close(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), documentCLIFrameBytes)
	draft := cliDocumentDraft{emit: emit}
	sessionID := ""
	toolNames := map[string]string{}
	toolsEnabled := false
	var runtimeErr error
	request := func(id int, method string, params any) (json.RawMessage, error) {
		if err := life.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			return nil, err
		}
		for scanner.Scan() {
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if !utf8.Valid(scanner.Bytes()) || json.Unmarshal(scanner.Bytes(), &msg) != nil {
				return nil, generationError(ErrorProtocol, "Kimi ACP 输出协议无效")
			}
			if msg.Method != "" && len(msg.ID) > 0 {
				response := map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32601, "message": "NeoRead does not expose this operation"}}
				if msg.Method == "session/request_permission" {
					delete(response, "error")
					response["result"] = kimiDocumentPermission(msg.Params, sessionID, toolsEnabled && child.Err() == nil)
				}
				if err := life.send(response); err != nil {
					return nil, err
				}
				continue
			}
			if msg.Method == "session/update" && id == 3 && sessionID != "" && child.Err() == nil {
				var update struct {
					Session string `json:"sessionId"`
					Update  struct {
						Type    string `json:"sessionUpdate"`
						ID      string `json:"toolCallId"`
						Title   string `json:"title"`
						Status  string `json:"status"`
						Content struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"content"`
					} `json:"update"`
				}
				// Tool updates use an array for content, message chunks an object.
				var envelope struct {
					Session string          `json:"sessionId"`
					Update  json.RawMessage `json:"update"`
				}
				_ = json.Unmarshal(msg.Params, &envelope)
				if envelope.Session == sessionID {
					var kind struct {
						Type string `json:"sessionUpdate"`
					}
					_ = json.Unmarshal(envelope.Update, &kind)
					if kind.Type == "agent_message_chunk" {
						if json.Unmarshal(msg.Params, &update) != nil {
							return nil, generationError(ErrorProtocol, "Kimi 文档片段格式无效")
						}
						if update.Update.Content.Type == "text" {
							runtimeErr = draft.add(update.Update.Content.Text)
						}
					} else if kind.Type == "tool_call" || kind.Type == "tool_call_update" {
						if !toolsEnabled {
							runtimeErr = generationError(ErrorCapability, "Kimi 在无工具模式下调用了工具")
						} else {
							runtimeErr = kimiDocumentToolUpdate(in.Chat, envelope.Update, toolNames, &draft)
						}
					}
					if runtimeErr != nil {
						// Keep draining the outstanding prompt so the lifecycle can
						// send session/cancel before its bounded forced teardown.
						cancel()
					}
				}
			}
			if string(msg.ID) == fmt.Sprint(id) {
				if runtimeErr != nil {
					return nil, runtimeErr
				}
				if child.Err() != nil {
					return nil, child.Err()
				}
				if len(msg.Error) > 0 && string(msg.Error) != "null" {
					if method == "session/new" {
						return nil, errKimiSessionRejected
					}
					return nil, generationError(ErrorUpstream, "Kimi 请求失败")
				}
				return msg.Result, nil
			}
		}
		if runtimeErr != nil {
			return nil, runtimeErr
		}
		if child.Err() != nil {
			return nil, child.Err()
		}
		return nil, generationError(ErrorNetwork, "Kimi ACP 连接中断")
	}
	init, err := request(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]string{"name": "neoread", "version": "1"}})
	if err != nil {
		return GenerateResult{}, err
	}
	var capabilities struct {
		Version int `json:"protocolVersion"`
		Agent   struct {
			MCP struct {
				HTTP bool `json:"http"`
			} `json:"mcpCapabilities"`
			Prompt struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	if json.Unmarshal(init, &capabilities) != nil || capabilities.Version != 1 {
		return GenerateResult{}, generationError(ErrorProtocol, "Kimi ACP 协商版本不支持")
	}
	if len(in.images()) > 0 && !capabilities.Agent.Prompt.Image {
		return GenerateResult{}, generationError(ErrorCapability, "当前 Kimi ACP 不支持图片输入")
	}
	mcpServers := []any{}
	mode, validation := "agent", "unsupported_http"
	if capabilities.Agent.MCP.HTTP {
		if err = checkBoundDocumentMCP(child, url, token); err != nil {
			return GenerateResult{}, err
		}
		mcpServers = append(mcpServers, map[string]any{"type": "http", "name": "neoread_document", "url": url, "headers": []any{map[string]string{"name": "Authorization", "value": "Bearer " + token}}})
		// ACP exposes no remote inventory/connection-status result. This is
		// deliberately not reported as an inventory validation pass.
		validation = "inventory_unavailable"
	} else {
		mode = "retrieval"
	}
	raw, err := request(2, "session/new", map[string]any{"cwd": work, "mcpServers": mcpServers})
	if errors.Is(err, errKimiSessionRejected) && mode == "agent" {
		mode, validation = "retrieval", "session_rejected"
		raw, err = request(5, "session/new", map[string]any{"cwd": work, "mcpServers": []any{}})
	}
	in.Chat.Agent.server.diagnosticEvent(slog.LevelInfo, "chat.document_tools", "provider", "kimi", "status", validation)
	if err != nil {
		return GenerateResult{}, err
	}
	var session struct {
		ID     string `json:"sessionId"`
		Models struct {
			Current string `json:"currentModelId"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &session) != nil || session.ID == "" {
		return GenerateResult{}, generationError(ErrorProtocol, "Kimi 会话无效")
	}
	sessionID = session.ID
	if emit != nil && session.Models.Current != "" {
		if err = emit(ProviderEvent{Metrics: []ModelTokens{{Model: session.Models.Current}}}); err != nil {
			return GenerateResult{}, err
		}
	}
	toolsEnabled = mode == "agent"
	life.setCancel(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": sessionID}})
	if mode == "retrieval" {
		a := in.Chat.Agent
		a.Mode, a.canViewPages = "retrieval", false
		stats, e := a.preRetrieveLibrary(child, in.Chat.Question)
		if e != nil {
			return GenerateResult{}, e
		}
		in.Prompt = apiDocumentInstructions(false, false) + "\n" + in.Prompt + a.libraryRetrievalContext(stats)
	}
	if in.Effort != "" {
		if _, err = request(4, "session/set_config_option", map[string]any{"sessionId": sessionID, "configId": "thinking", "value": in.Effort}); err != nil {
			return GenerateResult{}, err
		}
	}
	parts := []any{map[string]string{"type": "text", "text": in.Prompt}}
	for _, image := range in.images() {
		parts = append(parts, map[string]string{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(image)})
	}
	raw, err = request(3, "session/prompt", map[string]any{"sessionId": sessionID, "prompt": parts})
	if err != nil {
		return GenerateResult{}, err
	}
	var terminal struct {
		Stop string `json:"stopReason"`
	}
	if json.Unmarshal(raw, &terminal) != nil || terminal.Stop != "end_turn" {
		return GenerateResult{}, generationError(ErrorProtocol, "Kimi 未完成文档回答")
	}
	return draft.finish(ctx)
}

func kimiDocumentName(title string) string {
	// Current Kimi uses the bare MCP tool name, optionally ': <argument>'.
	// Accept exact names only; the server source cannot be attested by ACP.
	name := strings.SplitN(title, ": ", 2)[0]
	name = strings.TrimPrefix(name, "mcp__neoread_document__")
	name = strings.TrimPrefix(name, "neoread_document/")
	if documentToolName(name) {
		return name
	}
	return ""
}

func kimiDocumentPermission(raw json.RawMessage, session string, active bool) any {
	var request struct {
		Session string `json:"sessionId"`
		Tool    struct {
			Title string `json:"title"`
		} `json:"toolCall"`
		Options []struct {
			ID   string `json:"optionId"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	if json.Unmarshal(raw, &request) == nil && active && session != "" && request.Session == session && kimiDocumentName(request.Tool.Title) != "" {
		for _, option := range request.Options {
			if option.Kind == "allow_once" {
				return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": option.ID}}
			}
		}
	}
	return map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}
}

func kimiDocumentToolUpdate(chat *chatInput, raw json.RawMessage, names map[string]string, draft *cliDocumentDraft) error {
	var update struct {
		Type   string `json:"sessionUpdate"`
		ID     string `json:"toolCallId"`
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &update) != nil || update.ID == "" {
		return generationError(ErrorCapability, "Kimi 无法核验文档工具")
	}
	name := names[update.ID]
	if update.Title != "" {
		name = kimiDocumentName(update.Title)
	}
	if name == "" {
		return generationError(ErrorCapability, "Kimi 调用了未授权工具")
	}
	if update.Type == "tool_call" {
		if err := draft.reset(); err != nil {
			return err
		}
	}
	names[update.ID] = name
	status := "running"
	if update.Status == "completed" || update.Status == "failed" || update.Status == "cancelled" {
		status = update.Status
	}
	return chat.recordTool(ToolActivity{ID: update.ID, Kind: "tool", Tool: "neoread_document/" + name, Title: "调用工具", Status: status})
}
