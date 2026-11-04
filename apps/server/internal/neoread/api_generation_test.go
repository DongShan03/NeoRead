package neoread

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"neoread.local/server/internal/diagnostics"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func anthropicTestWire(text string) string {
	return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		fmt.Sprintf("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text) +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
}
func TestAnthropicStreamBoundaries(t *testing.T) {
	wire := anthropicTestWire("你好")
	for _, tt := range []struct {
		name, wire string
		kind       ErrorKind
	}{
		{"complete", wire, ""},
		{"missing-stop", strings.ReplaceAll(wire, "data: {\"type\":\"message_stop\"}\n\n", ""), ErrorProtocol},
		{"missing-reason", strings.ReplaceAll(wire, "\"end_turn\"", "null"), ErrorProtocol},
		{"empty", anthropicTestWire(""), ErrorProtocol},
		{"length", strings.ReplaceAll(wire, "end_turn", "max_tokens"), ErrorLimit},
		{"tool", strings.ReplaceAll(wire, "end_turn", "tool_use"), ErrorCapability},
		{"error-after-output", strings.ReplaceAll(wire, "data: {\"type\":\"message_stop\"}", "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"private\"}}"), ErrorUpstream},
		{"text-without-start", strings.ReplaceAll(wire, "\"index\":0,\"delta\"", "\"index\":1,\"delta\""), ErrorProtocol},
		{"duplicate-start", strings.ReplaceAll(wire, "data: {\"type\":\"content_block_stop\",\"index\":0}", "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}"), ErrorProtocol},
	} {
		t.Run(tt.name, func(t *testing.T) {
			text := ""
			var tokens *TokenCounts
			result, err := readAnthropicStream(t.Context(), oneByteReader{strings.NewReader(tt.wire)}, APIConnection{}, func(e ProviderEvent) error {
				text += e.Text
				if len(e.Metrics) > 0 {
					tokens = e.Metrics[0].Tokens
				}
				return nil
			})
			if tt.kind == "" {
				if err != nil || result.Text != text || text != "你好" || tokens == nil || tokens.TotalTokens != 12 {
					t.Fatalf("%+v %v %+v", result, err, tokens)
				}
			} else if err == nil || errorKind(err) != tt.kind {
				t.Fatalf("%v want %s", err, tt.kind)
			}
		})
	}
}

func TestAnthropicThinkingIsNotAnswerText(t *testing.T) {
	thinking := "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"private reasoning\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"private signature\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n"
	wire := strings.Replace(anthropicTestWire("answer"), "data: {\"type\":\"content_block_start\"", thinking+"data: {\"type\":\"content_block_start\"", 1)
	text := ""
	result, err := readAnthropicStream(t.Context(), strings.NewReader(wire), APIConnection{}, func(e ProviderEvent) error { text += e.Text; return nil })
	if err != nil || result.Text != "answer" || text != "answer" {
		t.Fatalf("%+v %v %s", result, err, text)
	}
}

func TestAnthropicDocumentRetrievalConversation(t *testing.T) {
	s, _ := prepareConversationFixture(t)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"data": []any{map[string]string{"id": "test"}}})
			return
		}
		calls++
		if r.URL.Path != "/messages" {
			t.Errorf("wrong protocol: %s", r.URL.Path)
		}
		var payload struct {
			System   string
			Messages []completionMessage
			Tools    []any
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if len(payload.Tools) > 0 || payload.System == "" {
			t.Error("invalid retrieval payload")
		}
		if !strings.Contains(jsonText(payload.Messages), "first-turn-marker") {
			t.Error("lost conversation history")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, anthropicTestWire("answer"))
	}))
	defer upstream.Close()
	configureDocumentAPI(t, s, upstream.URL)
	c := s.aiConfig()
	c.API.Protocol = "anthropic"
	passModels(&c, map[string]bool{"test": false})
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	for _, question := range []string{"first-turn-marker", "继续"} {
		r := conversationRequest(t, s, "api", question)
		if strings.Count(r.Body.String(), "event: done") != 1 {
			t.Fatal(r.Body)
		}
	}
	if calls != 2 {
		t.Fatalf("unexpected capability calls: %d", calls)
	}
	var raw string
	if err := s.Store.DB.QueryRow("SELECT body FROM document_conversations WHERE document_id='doc'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var state documentConversation
	if err := json.Unmarshal([]byte(raw), &state); err != nil || len(state.Messages) != 5 {
		t.Fatalf("conversation not persisted: %v %d", err, len(state.Messages))
	}
}

func TestAnthropicThinkingOutputSignal(t *testing.T) {
	for _, tt := range []struct {
		name, block, delta string
		want               int
	}{
		{"delta", `{"type":"thinking","thinking":""}`, `{"type":"thinking_delta","thinking":"private thought"}`, 1},
		{"initial", `{"type":"thinking","thinking":"private thought"}`, `{"type":"thinking_delta","thinking":"more private thought"}`, 1},
		{"empty", `{"type":"thinking","thinking":" "}`, `{"type":"thinking_delta","thinking":" "}`, 0},
		{"signature", `{"type":"thinking"}`, `{"type":"signature_delta","signature":"private signature"}`, 0},
		{"redacted", `{"type":"redacted_thinking","data":"private data"}`, "", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			thinking := fmt.Sprintf("data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":%s}\n\n", tt.block)
			if tt.delta != "" {
				thinking += fmt.Sprintf("data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":%s}\n\n", tt.delta)
			}
			thinking += "data: {\"type\":\"content_block_stop\",\"index\":1}\n\ndata: {\"type\":\"ping\"}\n\n"
			wire := strings.Replace(anthropicTestWire("answer"), "data: {\"type\":\"content_block_start\"", thinking+"data: {\"type\":\"content_block_start\"", 1)
			signals, text := 0, ""
			result, err := readAnthropicStream(t.Context(), strings.NewReader(wire), APIConnection{}, func(e ProviderEvent) error {
				if e.OutputObserved {
					signals++
				}
				encoded, _ := json.Marshal(e)
				if strings.Contains(string(encoded), "private") || e.Reasoning != "" {
					t.Fatalf("thinking leaked: %s", encoded)
				}
				text += e.Text
				return nil
			})
			if err != nil || signals != tt.want || text != "answer" || result.Text != "answer" || result.Reasoning != "" {
				t.Fatalf("signals=%d want=%d result=%+v err=%v", signals, tt.want, result, err)
			}
		})
	}
}

func TestProviderErrorsAreClassifiedAndRedacted(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	for _, tt := range []struct {
		url    string
		status int
		body   string
		kind   ErrorKind
		param  string
	}{
		{"https://api.openai.com/v1", 400, `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"reasoning_effort","message":"SECRET_KEY private paper"}}`, ErrorConfiguration, "reasoning_effort"},
		{"https://open.bigmodel.cn/api/paas/v4", 429, `{"error":{"code":"1113","message":"private paper"}}`, ErrorBilling, ""},
		{"https://open.bigmodel.cn/api/paas/v4", 429, `{"error":{"code":"1302"}}`, ErrorLimit, ""},
		{"https://api.openai.com/v1", 429, `{"error":{"code":"insufficient_quota","type":"invalid_request_error"}}`, ErrorBilling, ""},
		{"https://api.anthropic.com/v1", 200, `{"type":"error","error":{"type":"overloaded_error","message":"private paper"}}`, ErrorUpstream, ""},
		{"https://generativelanguage.googleapis.com/v1beta", 400, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"Invalid thinking.type private paper"}}`, ErrorConfiguration, "thinking.type"},
		{"https://api.minimax.io/v1", 200, `{"base_resp":{"status_code":2013,"status_msg":"Invalid reasoning_effort private paper"}}`, ErrorConfiguration, "reasoning_effort"},
		{"https://relay.test/v1", 429, `{"error":{"code":"1113"}}`, ErrorLimit, ""},
		{"https://relay.test/v1", 408, `request timeout`, ErrorTimeout, ""},
		{"https://relay.test/v1", 504, `<html>gateway timeout</html>`, ErrorTimeout, ""},
		{"https://relay.test/v1", 502, `<html>SECRET_KEY private paper</html>`, ErrorUpstream, ""},
		{"https://relay.test/v1", 403, `{"error":{"type":"permission_denied_error"}}`, ErrorPermission, ""},
		{"https://relay.test/v1", 400, `{"error":{"code":"private-paper","param":"private-paper","type":"private-paper"}}`, ErrorConfiguration, ""},
	} {
		t.Run(fmt.Sprint(tt.status)+tt.body, func(t *testing.T) {
			c := APIConnection{URL: tt.url, Key: "SECRET_KEY"}
			ctx, _ := traceAPI(t.Context(), c, "model")
			err := providerFailure(ctx, c, tt.status, http.Header{"X-Request-Id": []string{"req_123"}}, []byte(tt.body), "http")
			if errorKind(err) != tt.kind {
				t.Fatalf("%v want %s", err, tt.kind)
			}
			if tt.kind == ErrorTimeout && !strings.HasPrefix(err.Error(), "上游请求超时") {
				t.Fatalf("timeout has no readable explanation: %v", err)
			}
			if err.(*apiFailure).Param != tt.param {
				t.Fatal(err)
			}
			if strings.Contains(err.Error(), "SECRET_KEY") || strings.Contains(err.Error(), "private paper") {
				t.Fatal("error leaks content")
			}
		})
	}
	for _, secret := range []string{"SECRET_KEY", "private paper", "private-paper"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("log leaks", secret)
		}
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 13 {
		t.Fatal(len(lines))
	}
	for _, line := range lines {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil || event["apiRequestId"] == "" || event["upstreamRequestId"] != "req_123" {
			t.Fatal(line)
		}
	}
	// Exercise the real diagnostics writer's redaction too.
	redacted := string(diagnostics.RedactLog(logs.Bytes(), false, "SECRET_KEY"))
	if !strings.Contains(redacted, "unsupported_value") {
		t.Fatal("lost useful error code")
	}
}
func TestAPIErrorDoesNotRetryAndLogsEffort(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-Request-ID", "req_test")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"code":"unsupported_parameter","param":"reasoning_effort","message":"SECRET_KEY private question"}}`)
	}))
	defer server.Close()
	ctx := context.WithValue(t.Context(), apiAttemptKey{}, "attempt_test")
	_, err := streamCompletion(ctx, APIConnection{URL: server.URL, Key: "SECRET_KEY"}, completionRequest{Model: "claude-custom", Effort: "max"}, AIInput{Prompt: "private question"}, nil)
	if calls != 1 || errorKind(err) != ErrorConfiguration {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	for _, value := range []string{`"requestedEffort":"max"`, `"sentEffort":"high"`, `"attemptId":"attempt_test"`, `"upstreamCode":"unsupported_parameter"`, `"upstreamRequestId":"req_test"`, `"stage":"http"`} {
		if !strings.Contains(logs.String(), value) {
			t.Fatal("missing diagnostic", value)
		}
	}
	for _, value := range []string{"SECRET_KEY", "private question", server.URL} {
		if strings.Contains(logs.String(), value) {
			t.Fatal("leaked", value)
		}
	}
}

func TestHTTP200BusinessErrorIsNotSuccess(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"error\":{\"code\":\"unsupported_parameter\",\"param\":\"reasoning_effort\"}}\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"error":{"code":"unsupported_parameter","param":"reasoning_effort"}}`)
				}
			}))
			defer upstream.Close()
			result, err := streamCompletion(t.Context(), APIConnection{URL: upstream.URL}, completionRequest{Model: "test", Effort: "high"}, AIInput{Prompt: "private"}, nil)
			if result.Text != "" || errorKind(err) != ErrorConfiguration {
				t.Fatalf("%+v %v", result, err)
			}
			document, err := toolCompletion(t.Context(), APIConnection{URL: upstream.URL}, completionRequest{Model: "test", Effort: "high"}, []toolCompletionMessage{{Role: "user", Content: "private"}}, documentAPITools())
			if document.Text != "" || errorKind(err) != ErrorConfiguration {
				t.Fatalf("document: %+v %v", document, err)
			}
		})
	}
}

func TestBroadRequestErrorPreservesHTTPClassification(t *testing.T) {
	for _, tt := range []struct {
		status int
		want   ErrorKind
	}{
		{200, ErrorConfiguration}, {400, ErrorConfiguration}, {422, ErrorConfiguration},
		{401, ErrorAuthentication}, {403, ErrorPermission}, {402, ErrorBilling},
		{404, ErrorModel}, {413, ErrorContext}, {429, ErrorLimit}, {503, ErrorUpstream},
	} {
		for _, field := range []string{"type", "code"} {
			body := []byte(fmt.Sprintf(`{"error":{%q:"invalid_request_error"}}`, field))
			if field == "type" {
				body = []byte(`{"error":{"type":"invalid_request_error","code":"new_policy_code"}}`)
			}
			err := providerFailure(context.Background(), APIConnection{}, tt.status, nil, body, "http")
			if errorKind(err) != tt.want {
				t.Errorf("status=%d field=%s: %v want %s", tt.status, field, err, tt.want)
			}
		}
	}
}

func TestToolStreamRejectsBusinessErrorBeforeOutput(t *testing.T) {
	wire := "data: " + `{"base_resp":{"status_code":2013,"status_msg":"Invalid reasoning_effort private paper"},"choices":[{"index":0,"delta":{"content":"must not save"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	emitted := false
	result, err := readToolCompletionWithConnection(t.Context(), strings.NewReader(wire), APIConnection{URL: "https://api.minimax.io/v1"}, toolStreamCallbacks{Draft: func(string, bool) error { emitted = true; return nil }})
	if err == nil || errorKind(err) != ErrorConfiguration || emitted || result.Text != "" {
		t.Fatalf("accepted unsuccessful provider response: %+v %v emitted=%v", result, err, emitted)
	}
	if strings.Contains(err.Error(), "private paper") {
		t.Fatal("leaked upstream content")
	}
}

func writeAPIReply(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", text)
}

func TestCompletionStreamBoundaries(t *testing.T) {
	part := `data: {"choices":[{"index":0,"delta":{"content":"你 好\n"}}]}` + "\n\n"
	stop := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	end := "data: [DONE]\n\n"
	for _, tc := range []struct {
		name, wire string
		ok         bool
		kind       ErrorKind
	}{
		{"normal", part + stop + end, true, ""},
		{"crlf-comments-multiline", ": heartbeat\r\nevent: message\r\ndata: {\"choices\":\r\ndata: [{\"index\":0,\"delta\":{\"content\":\"你 好\\n\"}}]}\r\n\r\n" + stop + end, true, ""},
		{"other-choice", `data: {"choices":[{"index":1,"delta":{"content":"private"}},{"index":0,"delta":{"content":"你 好\n"}}]}` + "\n\n" + stop + end, true, ""},
		{"no-terminal", part, false, ErrorProtocol},
		{"no-done", part + stop, false, ErrorProtocol},
		{"no-reason", part + end, false, ErrorProtocol},
		{"empty", stop + end, false, ErrorProtocol},
		{"malformed", part + "data: {\n\n", false, ErrorProtocol},
		{"upstream-error", part + `data: {"error":{"message":"SECRET_KEY"}}` + "\n\n", false, ErrorUpstream},
		{"length", part + `data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" + end, false, ErrorLimit},
		{"refusal", `data: {"choices":[{"index":0,"delta":{"refusal":"no"}}]}` + "\n\n", false, ErrorUpstream},
		{"late", part + stop + part + end, false, ErrorProtocol},
		{"missing-index", `data: {"choices":[{"delta":{"content":"oops"}}]}` + "\n\n", false, ErrorProtocol},
		{"tool", `data: {"choices":[{"index":0,"delta":{"tool_calls":[{}]}}]}` + "\n\n", false, ErrorCapability},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var text string
			// One byte reads split UTF-8 and every SSE delimiter independently.
			result, err := readCompletionStream(t.Context(), oneByteReader{strings.NewReader(tc.wire)}, func(e ProviderEvent) error { text += e.Text; return nil })
			if tc.ok {
				if err != nil || result.Text != "你 好\n" || result.Text != text {
					t.Fatalf("%+v %q %v", result, text, err)
				}
			} else if err == nil || errorKind(err) != tc.kind || strings.Contains(err.Error(), "SECRET_KEY") {
				t.Fatalf("%v", err)
			}
		})
	}
	t.Run("escaped-frame", func(t *testing.T) {
		// Legal answer below 1 MiB with a wire representation above the old 2 MiB cap.
		wire := `data: {"choices":[{"index":0,"delta":{"content":"` + strings.Repeat(`\u0061`, 500000) + `"}}]}` + "\n\n" + stop + end
		result, err := readCompletionStream(t.Context(), strings.NewReader(wire), nil)
		if err != nil || len(result.Text) != 500000 {
			t.Fatalf("length=%d %v", len(result.Text), err)
		}
	})
	t.Run("answer-limit", func(t *testing.T) {
		wire := `data: {"choices":[{"index":0,"delta":{"content":"` + strings.Repeat("a", (1<<20)+1) + `"}}]}` + "\n\n"
		_, err := readCompletionStream(t.Context(), strings.NewReader(wire), nil)
		if errorKind(err) != ErrorLimit {
			t.Fatal(err)
		}
	})
	t.Run("frame-limit", func(t *testing.T) {
		_, err := readCompletionStream(t.Context(), strings.NewReader("data: "+strings.Repeat("a", maxProviderFrame)), nil)
		if err == nil {
			t.Fatal("accepted oversized frame")
		}
	})
}

type oneByteReader struct{ io.Reader }

func (r oneByteReader) Read(p []byte) (int, error) { return r.Reader.Read(p[:1]) }

func TestAPIHTTPBoundaries(t *testing.T) {
	for _, code := range []int{401, 429, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code); fmt.Fprint(w, "SECRET_KEY") }))
			defer upstream.Close()
			_, err := invokeAPI(t.Context(), APIConnection{URL: upstream.URL, Model: "test", Key: "SECRET_KEY"}, AIInput{Prompt: "test"}, nil)
			if err == nil || strings.Contains(err.Error(), "SECRET_KEY") {
				t.Fatal(err)
			}
		})
	}
	t.Run("redirect", func(t *testing.T) {
		var targetCalls atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
		defer target.Close()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
		defer upstream.Close()
		_, err := invokeAPI(t.Context(), APIConnection{URL: upstream.URL, Model: "test", Key: "secret"}, AIInput{Prompt: "test"}, nil)
		if err == nil || targetCalls.Load() != 0 {
			t.Fatal("redirect followed", err)
		}
	})
	t.Run("no-stream-retry", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			fmt.Fprint(w, `{"choices":[{"message":{"content":"answer"}}]}`)
		}))
		defer upstream.Close()
		_, err := invokeAPI(t.Context(), APIConnection{URL: upstream.URL, Model: "test"}, AIInput{Prompt: "test"}, nil)
		if err == nil || calls.Load() != 1 {
			t.Fatal("unexpected non-stream acceptance/retry", err)
		}
	})
	t.Run("emit-failure-cancels-http", func(t *testing.T) {
		stopped := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}}]}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(stopped)
		}))
		defer upstream.Close()
		failure := errors.New("consumer closed")
		_, err := (apiAdapter{APIConnection{URL: upstream.URL, Model: "test"}}).Stream(t.Context(), GenerateRequest{AIInput{Prompt: "test"}}, func(ProviderEvent) error { return failure })
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("HTTP body not closed")
		}
	})
}

func TestAPIFallbackStreamsOverHTTP(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "kimi"} {
		for _, vision := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/image=%t", provider, vision), func(t *testing.T) {
				fakeAgent(t, provider, "fail-before")
				gate := make(chan struct{})
				var released sync.Once
				defer released.Do(func() { close(gate) })
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						Stream   bool `json:"stream"`
						N        int  `json:"n"`
						Messages []struct {
							Content json.RawMessage `json:"content"`
						} `json:"messages"`
					}
					if json.NewDecoder(r.Body).Decode(&body) != nil || !body.Stream || body.N != 1 || len(body.Messages) != 2 {
						t.Error("not a streaming request")
						return
					}
					if vision && !strings.Contains(string(body.Messages[1].Content), "data:image/png;base64,") {
						t.Error("missing image")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"首段\"}}]}\n\n")
					w.(http.Flusher).Flush()
					select {
					case <-gate:
					case <-r.Context().Done():
						return
					case <-time.After(3 * time.Second):
						t.Error("client did not receive first delta")
						return
					}
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"尾段\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}))
				defer upstream.Close()
				s, _ := imageFixture(t)
				config := AIConfig{Primary: provider, Models: map[string]string{}, TextAPI: APIConnection{URL: upstream.URL, Model: "test"}, ImageAPI: APIConnection{URL: upstream.URL, Model: "test"}, Capabilities: map[string]Capability{}}
				for _, p := range []string{provider, "text-api", "image-api"} {
					config.Capabilities[p] = Capability{Text: true, Vision: true, Fingerprint: configPrint(config, p)}
				}
				if err := s.writeAIConfig(config); err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(s.Handler())
				defer server.Close()
				body := map[string]any{"provider": provider, "prompt": "test", "context": ""}
				if vision {
					body["attachments"] = []string{"p1-b1"}
				}
				raw, _ := json.Marshal(body)
				ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/documents/doc/chat", bytes.NewReader(raw))
				req.Header.Set("Authorization", "Bearer test-secret")
				res, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				scan := bufio.NewScanner(res.Body)
				event, text := "", ""
				fallback, done := 0, 0
				for scan.Scan() {
					line := scan.Text()
					if strings.HasPrefix(line, "event: ") {
						event = strings.TrimPrefix(line, "event: ")
					}
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					switch event {
					case "error":
						t.Fatal(line)
					case "fallback":
						fallback++
					case "done":
						done++
					case "delta":
						if fallback != 1 {
							t.Fatal("delta before fallback")
						}
						var e struct {
							Text string `json:"text"`
						}
						if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e) != nil {
							t.Fatal(line)
						}
						text += e.Text
						if text == "首段" {
							var count int
							if err := s.Store.DB.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil || count != 1 {
								t.Fatal("premature save", count, err)
							}
							released.Do(func() { close(gate) })
						}
					}
				}
				if scan.Err() != nil || done != 1 || text != "首段尾段" {
					t.Fatalf("done=%d text=%q err=%v", done, text, scan.Err())
				}
				var count int
				if err := s.Store.DB.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil || count != 2 {
					t.Fatal(count, err)
				}
			})
		}
	}
}

func TestAPIEndpointSelection(t *testing.T) {
	for _, tt := range []struct{ url, override, vendor, protocol, base string }{
		{"https://api.openai.com", "", "openai", "openai", "https://api.openai.com/v1"},
		{"https://api.anthropic.com", "", "anthropic", "anthropic", "https://api.anthropic.com/v1"},
		{"https://generativelanguage.googleapis.com/v1beta", "", "google", "openai", "https://generativelanguage.googleapis.com/v1beta/openai"},
		{"https://api.moonshot.cn/v1", "", "moonshot", "openai", "https://api.moonshot.cn/v1"},
		{"https://open.bigmodel.cn/api/coding/paas/v4/", "", "zhipu", "openai", "https://open.bigmodel.cn/api/coding/paas/v4"},
		{"https://api.deepseek.com", "", "deepseek", "openai", "https://api.deepseek.com"},
		{"https://api.minimaxi.com", "", "minimax", "openai", "https://api.minimaxi.com/v1"},
		{"https://api.anthropic.com.evil.test/v1", "", "custom", "openai", "https://api.anthropic.com.evil.test/v1"},
		{"https://relay.test/claude", "", "custom", "openai", "https://relay.test/claude"},
		{"https://relay.test/v1", "anthropic", "custom", "anthropic", "https://relay.test/v1"},
		{"https://api.anthropic.com/v1", "openai", "anthropic", "openai", "https://api.anthropic.com/v1"},
		{"https://api.openai.com:8443/v1", "", "custom", "openai", "https://api.openai.com:8443/v1"},
		{"https://api.openai.com/not-api", "", "custom", "openai", "https://api.openai.com/not-api"},
	} {
		t.Run(tt.url+tt.override, func(t *testing.T) {
			got := resolveAPIEndpoint(APIConnection{URL: tt.url, Protocol: tt.override})
			want := apiEndpoint{tt.vendor, tt.protocol, tt.base}
			if got != want {
				t.Fatalf("%+v want %+v", got, want)
			}
		})
	}
}

type apiRoundTripper func(*http.Request) (*http.Response, error)

func (f apiRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Capture the actual HTTP payload with official URLs, without real credentials
// or external network calls. Tests in this package do not run in parallel.
func TestSevenProviderWireParameters(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, tt := range []struct{ url, model, level, field, value, thinking string }{
		{"https://api.openai.com", "gpt-5", "medium", "reasoning_effort", "medium", ""},
		{"https://api.anthropic.com", "claude-sonnet-4-6", "high", "output_config", "high", "adaptive"},
		{"https://generativelanguage.googleapis.com/v1beta", "gemini-3-pro-preview", "medium", "reasoning_effort", "high", ""},
		{"https://api.moonshot.ai/v1", "kimi-k3", "medium", "reasoning_effort", "high", ""},
		{"https://api.moonshot.ai/v1", "kimi-k2.6", "high", "", "", "enabled"},
		{"https://api.z.ai/api/paas/v4", "glm-5.3", "medium", "reasoning_effort", "high", "enabled"},
		{"https://api.z.ai/api/paas/v4", "glm-5.2", "low", "reasoning_effort", "high", "enabled"},
		{"https://api.deepseek.com", "deepseek-flash", "medium", "reasoning_effort", "high", "enabled"},
		{"https://api.minimax.io/v1", "MiniMax-M3.1-Flash-Preview", "max", "reasoning_effort", "max", "adaptive"},
		{"https://api.minimax.io/v1", "MiniMax-M2.7", "high", "", "", ""},
		{"https://relay.test/v1", "claude-sonnet-4-6", "medium", "reasoning_effort", "medium", ""},
		{"https://relay.test/v1", "gemini-3-pro-preview", "medium", "reasoning_effort", "medium", ""},
		{"https://api.openai.com/v1", "gpt-4o", "high", "", "", ""},
		{"https://api.openai.com/v1", "future-model", "high", "", "", ""},
	} {
		t.Run(tt.model+tt.url, func(t *testing.T) {
			calls := 0
			http.DefaultTransport = apiRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if _, ok := body["temperature"]; ok {
					t.Fatal("unexpected sampling override")
				}
				if tt.field == "" {
					if _, ok := body["reasoning_effort"]; ok {
						t.Fatal("sent unsupported effort")
					}
				} else if tt.field == "output_config" {
					if body[tt.field].(map[string]any)["effort"] != tt.value {
						t.Fatal(body[tt.field])
					}
					if req.URL.Path != "/v1/messages" || req.Header.Get("x-api-key") != "test-key" || req.Header.Get("Authorization") != "" || req.Header.Get("anthropic-version") == "" {
						t.Fatal("wrong native authentication or path")
					}
					for _, key := range []string{"reasoning_effort", "stream_options", "n"} {
						if _, ok := body[key]; ok {
							t.Fatal("OpenAI field in native request", key)
						}
					}
				} else if body[tt.field] != tt.value {
					t.Fatalf("%s = %v want %s", tt.field, body[tt.field], tt.value)
				}
				thinking, ok := body["thinking"].(map[string]any)
				if tt.thinking == "" && ok {
					t.Fatal("unexpected thinking", thinking)
				}
				if tt.thinking != "" && thinking["type"] != tt.thinking {
					t.Fatal(thinking)
				}
				wire := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
				if tt.field == "output_config" {
					wire = anthropicTestWire("ok")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}, nil
			})
			result, err := streamCompletion(t.Context(), APIConnection{URL: tt.url, Key: "test-key"}, completionRequest{Model: tt.model, Effort: tt.level}, AIInput{Prompt: "private"}, nil)
			if err != nil || result.Text != "ok" || calls != 1 {
				t.Fatalf("%+v %v calls=%d", result, err, calls)
			}
			document, err := toolCompletion(t.Context(), APIConnection{URL: tt.url, Key: "test-key"}, completionRequest{Model: tt.model, Effort: tt.level}, []toolCompletionMessage{{Role: "system", Content: "document instructions"}, {Role: "user", Content: "private"}}, nil)
			if err != nil || document.Text != "ok" || calls != 2 {
				t.Fatalf("document request: %+v %v calls=%d", document, err, calls)
			}
		})
	}
}
func TestUnknownAdvertisedEffortAndFingerprint(t *testing.T) {
	e := resolveAPIEndpoint(APIConnection{URL: "https://relay.test/v1"})
	r := reasoningRule(e, "gpt-5", []string{"off", "on"})
	if r.resolve("medium") != "" || r.info().Mode != "unknown" {
		t.Fatal(r)
	}
	c := AIConfig{API: APIConnection{URL: "https://relay.test/v1"}}
	old := configPrint(c, "api:model")
	c.API.Protocol = "anthropic"
	if configPrint(c, "api:model") == old {
		t.Fatal("protocol did not invalidate capability")
	}
}
func TestCustomAnthropicProtocolAndImage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			t.Fatal(r.URL.Path)
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["output_config"].(map[string]any)["effort"] != "medium" {
			t.Fatal(payload)
		}
		messages := payload["messages"].([]any)
		parts := messages[0].(map[string]any)["content"].([]any)
		if parts[1].(map[string]any)["type"] != "image" {
			t.Fatal(parts)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, anthropicTestWire("ok"))
	}))
	defer upstream.Close()
	_, err := streamCompletion(context.Background(), APIConnection{URL: upstream.URL, Protocol: "anthropic"}, completionRequest{Model: "custom", Effort: "medium"}, AIInput{Prompt: "image", Image: []byte{1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestProviderModelBoundaries(t *testing.T) {
	for _, tt := range []struct{ url, model, mode, requested, want string }{
		{"https://api.openai.com", "gpt-6.1-sol", "levels", "max", "max"},
		{"https://api.openai.com", "gpt-5.5", "levels", "max", "xhigh"},
		{"https://generativelanguage.googleapis.com", "gemini-2.5-flash-image", "unknown", "medium", ""},
		{"https://api.minimax.io", "MiniMax-M2-future", "unknown", "high", ""},
		{"https://api.anthropic.com", "claude-haiku-4-5-20251001", "budget", "high", "8192"},
	} {
		r := reasoningRule(resolveAPIEndpoint(APIConnection{URL: tt.url}), tt.model, nil)
		if r.Mode != tt.mode || r.resolve(tt.requested) != tt.want {
			t.Fatalf("%s: %+v", tt.model, r)
		}
	}
	r := reasoningRule(resolveAPIEndpoint(APIConnection{URL: "https://api.moonshot.ai"}), "kimi-k2-thinking", nil)
	if r.Mode != "fixed" || r.Thinking != "" {
		t.Fatalf("K2 Thinking must not receive a hybrid thinking toggle: %+v", r)
	}
}

func TestKimiTranslationRetainsReasoningOnlyAfterCompletion(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	calls := 0
	http.DefaultTransport = apiRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		var payload struct {
			Messages []completionMessage `json:"messages"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if calls > 1 {
			if len(payload.Messages) != 4 || payload.Messages[2].Reasoning != "private reasoning" {
				t.Fatalf("missing preserved reasoning: %+v", payload.Messages)
			}
		}
		wire := "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"private reasoning\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		if calls == 2 {
			wire = strings.ReplaceAll(wire, "data: [DONE]\n\n", "")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}, nil
	})
	adapter := apiAgentAdapter{conn: APIConnection{URL: "https://api.moonshot.ai/v1"}, text: apiModelChoice{"kimi-k3", "high"}, catalog: func(context.Context) ([]AgentModel, error) { return nil, fmt.Errorf("catalog unavailable") }}
	session := apiTranslationSession{apiAgentAdapter: adapter}
	if _, err := session.Stream(t.Context(), GenerateRequest{AIInput{Prompt: "batch1"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Stream(t.Context(), GenerateRequest{AIInput{Prompt: "batch2"}}, nil); err == nil {
		t.Fatal("incomplete response accepted")
	}
	if len(session.history) != 2 || session.history[1].Reasoning != "private reasoning" {
		t.Fatal("failed turn entered history")
	}
	session.close()
	if len(session.history) != 0 || session.size != 0 {
		t.Fatal("history not released")
	}
}

func TestClaudeFableAndMythosEffort(t *testing.T) {
	for _, model := range []string{"claude-fable-5-1", "claude-mythos-5-1", "claude-fable-5", "claude-mythos-5"} {
		for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
			r := reasoningRule(resolveAPIEndpoint(APIConnection{URL: "https://api.anthropic.com"}), model, nil)
			p := map[string]any{}
			r.apply(p, "anthropic", level, r.resolve(level))
			if r.Mode != "levels" || r.info().Mappings["medium"] != "medium" || p["output_config"].(map[string]string)["effort"] != level || p["thinking"].(map[string]any)["type"] != "adaptive" || p["max_tokens"] != 32768 {
				t.Fatalf("%s %s: %+v", model, level, p)
			}
			relay := reasoningRule(resolveAPIEndpoint(APIConnection{URL: "https://relay.test/v1"}), model, nil)
			if relay.Source == "official" || relay.Thinking != "" || relay.resolve("max") != "high" {
				t.Fatal(relay)
			}
		}
	}
}

func TestDeepSeekReasoningStreamsSeparatelyFromAnswer(t *testing.T) {
	s, _ := prepareConversationFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"data": []any{map[string]string{"id": "test"}}})
			return
		}
		writeConversationReply(w, "最终答案", "先核对原文，再回答。")
	}))
	defer upstream.Close()
	configureDocumentAPI(t, s, upstream.URL)
	out := conversationRequest(t, s, "api", "请解释")
	if !strings.Contains(out.Body.String(), "event: reasoning") {
		t.Fatal("thinking deltas never reached the renderer", out.Body.String())
	}
	history, _ := s.Store.chatHistory("doc")
	raw := jsonText(history[len(history)-1])
	if !strings.Contains(raw, `"reasoning"`) || !strings.Contains(raw, "先核对原文") || strings.Contains(history[len(history)-1].Content, "先核对原文") {
		t.Fatal("thinking not saved separately", raw)
	}
	if strings.Contains(jsonText(conversationHistory(history)), "先核对原文") {
		t.Fatal("displayed thinking duplicated into question history")
	}
}

func TestAPIProbeTimeoutOverride(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(1100 * time.Millisecond):
			writeAPIReply(w, "NEOREAD_OK")
		}
	}))
	defer upstream.Close()
	s := testServer(t)
	c := s.aiConfig()
	c.API = APIConnection{URL: upstream.URL}
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	for _, seconds := range []string{"1", "2"} {
		w := request(t, s, "POST", "/api/ai/test/api?model=test&capability=text&timeoutSeconds="+seconds, nil)
		var cap Capability
		if err := json.Unmarshal(w.Body.Bytes(), &cap); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || cap.Text != (seconds == "2") {
			t.Fatalf("timeout=%s: %s", seconds, w.Body.String())
		}
		if seconds == "1" && !strings.Contains(cap.Error, "超时") {
			t.Fatal(cap.Error)
		}
	}
}

func TestAPIProbeRejectsInvalidTimeout(t *testing.T) {
	s := testServer(t)
	for _, value := range []string{"-1", "601", "1.5", "invalid"} {
		w := request(t, s, "POST", "/api/ai/test/api?model=test&capability=text&timeoutSeconds="+value, nil)
		if w.Code != 400 {
			t.Fatalf("timeout=%s: status=%d %s", value, w.Code, w.Body.String())
		}
	}
	for _, value := range []string{"", "0", "600"} {
		w := request(t, s, "POST", "/api/ai/test/api?model=test&capability=text&timeoutSeconds="+value, nil)
		if w.Code != 200 {
			t.Fatalf("valid timeout=%s: %s", value, w.Body.String())
		}
	}
}

func TestAPIProbeFirstOutput(t *testing.T) {
	var fail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			time.Sleep(200 * time.Millisecond)
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		if fail.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"Checking\"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		writeAPIReply(w, "NEOREAD_OK")
	}))
	defer upstream.Close()
	s := testServer(t)
	c := s.aiConfig()
	c.API = APIConnection{URL: upstream.URL}
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	w := request(t, s, "POST", "/api/ai/test/api?model=test&capability=text", nil)
	elapsed := time.Since(started).Milliseconds()
	var text map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &text); err != nil {
		t.Fatal(err)
	}
	first, ok := text["textFirstOutputMs"].(float64)
	if w.Code != 200 || text["text"] != true || !ok || first < 20 || float64(elapsed)-first < 300 {
		t.Fatalf("first output must exclude catalog lookup and wait for content, elapsed=%d: %s", elapsed, w.Body.String())
	}
	w = request(t, s, "POST", "/api/ai/test/api?model=test&capability=vision", nil)
	var vision map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &vision)
	if vision["textFirstOutputMs"] != first || vision["visionFirstOutputMs"] == nil {
		t.Fatalf("stage timings lost: %s", w.Body.String())
	}
	fail.Store(true)
	w = request(t, s, "POST", "/api/ai/test/api?model=test&capability=text", nil)
	var failed map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &failed)
	if failed["textFirstOutputMs"] != nil || failed["visionFirstOutputMs"] != nil {
		t.Fatalf("stale timings retained: %s", w.Body.String())
	}
}

func TestAnthropicProbeFirstThinkingOutput(t *testing.T) {
	for _, tt := range []struct {
		name                    string
		initial, timeout, empty bool
	}{
		{name: "thinking-delta"},
		{name: "thinking-start", initial: true},
		{name: "thinking-then-timeout", timeout: true},
		{name: "empty-then-timeout", timeout: true, empty: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					fmt.Fprint(w, `{"data":[]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				prefix, rest, _ := strings.Cut(anthropicTestWire("NEOREAD_OK"), "\n\n")
				fmt.Fprint(w, prefix+"\n\ndata: {\"type\":\"ping\"}\n\n")
				w.(http.Flusher).Flush()
				time.Sleep(100 * time.Millisecond)
				thought := "private thought"
				if tt.empty {
					thought = " "
				}
				if tt.initial {
					fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"thinking\",\"thinking\":%q}}\n\n", thought)
				} else {
					fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n")
					fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":%q}}\n\n", thought)
				}
				fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"private signature\"}}\n\n")
				w.(http.Flusher).Flush()
				if tt.timeout {
					<-r.Context().Done()
					return
				}
				time.Sleep(200 * time.Millisecond)
				fmt.Fprint(w, "data: {\"type\":\"content_block_stop\",\"index\":1}\n\n"+rest)
			}))
			defer upstream.Close()
			s := testServer(t)
			c := s.aiConfig()
			c.API = APIConnection{URL: upstream.URL, Protocol: "anthropic"}
			if err := s.writeAIConfig(c); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			w := request(t, s, "POST", "/api/ai/test/api?model=test&capability=text&timeoutSeconds=1", nil)
			elapsed := time.Since(started).Milliseconds()
			var cap Capability
			if err := json.Unmarshal(w.Body.Bytes(), &cap); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || cap.Text == tt.timeout {
				t.Fatalf("unexpected probe outcome: %s", w.Body.String())
			}
			if tt.empty {
				if cap.TextFirstOutputMs != nil {
					t.Fatalf("empty events counted as output: %s", w.Body.String())
				}
			} else if cap.TextFirstOutputMs == nil || *cap.TextFirstOutputMs < 50 || elapsed-*cap.TextFirstOutputMs < 150 {
				t.Fatalf("expected first thinking before answer/timeout, elapsed=%d: %s", elapsed, w.Body.String())
			}
			if tt.timeout && !strings.Contains(cap.Error, "超时") {
				t.Fatalf("missing timeout: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), "private") {
				t.Fatal("probe leaked thinking")
			}
		})
	}
}
