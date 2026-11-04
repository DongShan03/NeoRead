package neoread

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const deepSeekModels = `{"object":"list","data":[{"id":"deepseek-flash","object":"model","name":"DeepSeek-V4.1-Flash","effort":{"supported_levels":["low","high","max"],"default_level":"high"}},{"id":"deepseek-v4-pro","object":"model","name":"DeepSeek-V4-Pro","effort":{"supported_levels":["low","high","max"],"default_level":"high"}},{"id":"plain","object":"model"}]}`

type apiCall struct {
	Model    string              `json:"model"`
	Effort   string              `json:"reasoning_effort"`
	Messages []completionMessage `json:"messages"`
}

// fakeAPI serves /models and records every completion request. reply returns
// the answer text, or "" to fail the request with HTTP 500.
func fakeAPI(t *testing.T, reply func(int, apiCall) string) (*httptest.Server, func() []apiCall) {
	var mu sync.Mutex
	var calls []apiCall
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(401)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/v1/models" {
			io.WriteString(w, deepSeekModels)
			return
		}
		var call apiCall
		if r.URL.Path != "/v1/chat/completions" || json.NewDecoder(r.Body).Decode(&call) != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		calls = append(calls, call)
		n := len(calls)
		mu.Unlock()
		text := reply(n, call)
		if text == "" {
			w.WriteHeader(500)
			return
		}
		writeAPIReply(w, text)
	}))
	t.Cleanup(upstream.Close)
	return upstream, func() []apiCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]apiCall{}, calls...)
	}
}

// passModels records check results the way Settings would after testing.
func passModels(c *AIConfig, results map[string]bool) {
	if c.Capabilities == nil {
		c.Capabilities = map[string]Capability{}
	}
	for id, vision := range results {
		key := apiCapabilityKey(id)
		c.Capabilities[key] = Capability{Text: true, Vision: vision, CheckedAt: "checked", Fingerprint: configPrint(*c, key)}
	}
}

func TestAPIAgentUsesOnlyCheckedModels(t *testing.T) {
	s := testServer(t)
	upstream, calls := fakeAPI(t, func(int, apiCall) string { return "ok" })
	config := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL + "/v1", Key: "sk-test"}, Models: map[string]string{}, APIModels: []string{"added-model"}}
	// Before any check, nothing is usable and nothing is guessed.
	if apiCapable(config, taskChat, false) || s.generationService(config).ValidateInput(AIInput{Prompt: "hi"}, true) == nil {
		t.Fatal("an unchecked API agent was accepted")
	}
	passModels(&config, map[string]bool{"deepseek-flash": true, "deepseek-v4-pro": false})
	image := [][]byte{{1}}
	cases := []struct {
		task   modelTask
		images [][]byte
		want   string
		effort string
	}{
		{taskChat, nil, "deepseek-v4-pro", "high"},
		{taskTranslation, nil, "deepseek-flash", "high"},
		// Image input goes to the vision model, never the text-only Pro.
		{taskChat, image, "deepseek-flash", "high"},
	}
	for _, c := range cases {
		if _, err := s.apiAgentAdapter(config, c.task).Stream(t.Context(), GenerateRequest{AIInput{Prompt: "hi", Images: c.images}}, nil); err != nil {
			t.Fatal(err)
		}
		got := calls()[len(calls())-1]
		if got.Model != c.want || got.Effort != c.effort {
			t.Fatalf("%v images=%d: sent model %q effort %q", c.task, len(c.images), got.Model, got.Effort)
		}
	}
	// A text-only vision choice is refused; an unchecked chat choice too.
	config.VisionModels = map[string]string{"api": "deepseek-v4-pro"}
	config.Models["api"] = "plain"
	if apiCapable(config, taskChat, true) || apiCapable(config, taskChat, false) || !apiCapable(config, taskTranslation, false) {
		t.Fatal("choices bypassed their model checks")
	}
	// Models without effort metadata use the standard low/medium/high tiers.
	passModels(&config, map[string]bool{"plain": false})
	config.Efforts = map[string]map[string]string{"api": {"plain": "max"}}
	if _, err := s.apiAgentAdapter(config, taskChat).Stream(t.Context(), GenerateRequest{AIInput{Prompt: "hi"}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := calls()[len(calls())-1]; got.Model != "plain" || got.Effort != "high" {
		t.Fatalf("chosen model: %+v", got)
	}
	models, err := s.apiAgentModels(t.Context(), config)
	if err != nil || len(models) != 4 || !models[3].Custom || models[3].Capability != nil || models[0].Capability == nil || !models[0].Capability.Vision {
		t.Fatalf("models: %+v %v", models, err)
	}
	recommended := map[string]string{}
	for _, m := range models {
		for _, task := range m.RecommendedFor {
			recommended[task] = m.ID
		}
	}
	if recommended["chat"] != "deepseek-v4-pro" || recommended["translation"] != "deepseek-flash" || recommended["vision"] != "deepseek-flash" {
		t.Fatalf("recommendations: %v", recommended)
	}
	other := APIConnection{URL: upstream.URL + "/v1", Key: "sk-wrong"}
	if _, err = s.apiCatalog(other)(t.Context()); errorKind(err) != ErrorAuthentication {
		t.Fatalf("a different key reused the cached catalog: %v", err)
	}
}

func TestAPIAgentWithoutVisionModel(t *testing.T) {
	s := testServer(t)
	upstream, _ := fakeAPI(t, func(int, apiCall) string { return "ok" })
	config := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL + "/v1", Key: "sk-test"}, Models: map[string]string{}}
	passModels(&config, map[string]bool{"deepseek-v4-pro": false})
	service := s.generationService(config)
	if service.ValidateInput(AIInput{Prompt: "hi"}, true) != nil {
		t.Fatal("text chat refused")
	}
	if err := service.ValidateInput(AIInput{Images: [][]byte{{1}}}, true); errorKind(err) != ErrorCapability {
		t.Fatalf("image input accepted without a vision model: %v", err)
	}
	if _, err := s.apiAgentAdapter(config, taskChat).Stream(t.Context(), GenerateRequest{AIInput{Prompt: "hi", Images: [][]byte{{1}}}}, nil); errorKind(err) != ErrorCapability {
		t.Fatalf("adapter picked a model without vision: %v", err)
	}
}

func TestAPIModelCheckIsPerModel(t *testing.T) {
	s := testServer(t)
	upstream, calls := fakeAPI(t, func(_ int, call apiCall) string {
		if call.Model == "broken" {
			return ""
		}
		return "NEOREAD_OK"
	})
	config := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL + "/v1", Key: "sk-test"}, Models: map[string]string{}}
	if err := s.writeAIConfig(config); err != nil {
		t.Fatal(err)
	}
	if w := request(t, s, "POST", "/api/ai/test/api?capability=text", nil); w.Code != 400 {
		t.Fatal("a check without a model was accepted")
	}
	for _, model := range []string{"deepseek-flash", "broken"} {
		if w := request(t, s, "POST", "/api/ai/test/api?capability=text&model="+model, nil); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	saved := s.aiConfig()
	if !capable(saved, "api:deepseek-flash", false) || capable(saved, "api:broken", false) || saved.Capabilities["api:broken"].Error == "" {
		t.Fatalf("per-model results: %+v", saved.Capabilities)
	}
	if got := calls(); got[0].Model != "deepseek-flash" || got[0].Effort != "high" || got[1].Model != "broken" {
		t.Fatalf("probes: %+v", got)
	}
}

func TestAPITranslationSessionSpansBatches(t *testing.T) {
	s := testServer(t)
	upstream, calls := fakeAPI(t, func(n int, _ apiCall) string {
		if n == 2 {
			return ""
		}
		return "answer-" + string(rune('0'+n))
	})
	config := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL + "/v1", Key: "sk-test"}, Models: map[string]string{}}
	passModels(&config, map[string]bool{"deepseek-flash": true})
	service, sessions := s.translationService(config)
	defer sessions.close()
	session := service.connections["api"].Adapter
	if session != sessions.api {
		t.Fatal("translation does not use the worker's API session")
	}
	for i, prompt := range []string{"batch-1", "batch-2", "batch-3"} {
		_, err := session.Stream(t.Context(), GenerateRequest{AIInput{Prompt: prompt}}, nil)
		if (i == 1) != (err != nil) {
			t.Fatalf("batch %d: %v", i+1, err)
		}
	}
	got := calls()
	roles := func(c apiCall) string {
		parts := []string{}
		for _, m := range c.Messages {
			text, _ := m.Content.(string)
			if m.Role == "system" {
				text = "-"
			}
			parts = append(parts, m.Role+":"+text)
		}
		return strings.Join(parts, " ")
	}
	want := []string{
		"system:- user:batch-1",
		"system:- user:batch-1 assistant:answer-1 user:batch-2",
		// The failed second turn is dropped; the first one stays in the session.
		"system:- user:batch-1 assistant:answer-1 user:batch-3",
	}
	for i := range want {
		if roles(got[i]) != want[i] || got[i].Model != "deepseek-flash" {
			t.Fatalf("call %d: %s (%s)", i+1, roles(got[i]), got[i].Model)
		}
	}
	sessions.close()
	if sessions.api.history != nil {
		t.Fatal("closed session kept history")
	}
}

func TestAPITranslationHistoryTrimsOldestTurns(t *testing.T) {
	s := &apiTranslationSession{}
	turn := strings.Repeat("x", apiTranslationHistoryBytes/8)
	for i := 0; i < 8; i++ {
		s.record(turn, turn+string(rune('a'+i)))
	}
	if s.size > apiTranslationHistoryBytes || len(s.history)%2 != 0 || s.history[0].Role != "user" {
		t.Fatalf("history not trimmed to whole turns: %d bytes, %d messages", s.size, len(s.history))
	}
	if last := s.history[len(s.history)-1].Content.(string); !strings.HasSuffix(last, "h") {
		t.Fatal("latest turn was dropped")
	}
	huge := strings.Repeat("y", apiTranslationHistoryBytes)
	s.record(huge, "z")
	if len(s.history) != 2 || s.history[1].Content != "z" {
		t.Fatal("an oversized latest turn must stay as the only context")
	}
}

func TestAPIAgentConfigKeepsKeyPrivate(t *testing.T) {
	s := testServer(t)
	body := `{"primary":"api","models":{"api":"deepseek-flash"},"api":{"url":"https://api.deepseek.com","key":"sk-private","hasKey":false},"textAPI":{"url":"","model":"","hasKey":false},"imageAPI":{"url":"","model":"","hasKey":false},"capabilities":{}}`
	if w := request(t, s, "PUT", "/api/ai/config", strings.NewReader(body)); w.Code != 200 || strings.Contains(w.Body.String(), "sk-private") || !strings.Contains(w.Body.String(), `"hasKey":true`) {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	c := s.aiConfig()
	passModels(&c, map[string]bool{"deepseek-flash": false})
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	// The UI resends hasKey without the secret; the key and its test survive.
	resend := strings.Replace(body, `"key":"sk-private","hasKey":false`, `"hasKey":true`, 1)
	if w := request(t, s, "PUT", "/api/ai/config", strings.NewReader(resend)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if c = s.aiConfig(); c.API.Key != "sk-private" || !capable(c, "api:deepseek-flash", false) {
		t.Fatalf("key or capability lost: %+v", c.API)
	}
	// A new key is a new connection and must be tested again.
	changed := strings.Replace(body, "sk-private", "sk-other", 1)
	if w := request(t, s, "PUT", "/api/ai/config", strings.NewReader(changed)); w.Code != 200 || capable(s.aiConfig(), "api:deepseek-flash", false) {
		t.Fatal("capability survived a key change")
	}
	insecure := strings.Replace(body, "https://api.deepseek.com", "http://api.deepseek.com", 1)
	if w := request(t, s, "PUT", "/api/ai/config", strings.NewReader(insecure)); w.Code != 400 {
		t.Fatal("plain HTTP to a remote API was accepted")
	}
	w := request(t, s, "GET", "/api/providers?auth=skip", nil)
	if !strings.Contains(w.Body.String(), `{"id":"api","installed":true,"authenticated":true,"status":"已配置"}`) {
		t.Fatal(w.Body.String())
	}
}

// NEOREAD_TEST_DEEPSEEK=1 runs a real translation batch and a follow-up turn on
// the same session with DEEPSEEK_API_KEY.
func TestLiveDeepSeekTranslationSession(t *testing.T) {
	key := os.Getenv("DEEPSEEK_API_KEY")
	if os.Getenv("NEOREAD_TEST_DEEPSEEK") != "1" || key == "" {
		t.Skip("set NEOREAD_TEST_DEEPSEEK=1 and DEEPSEEK_API_KEY")
	}
	s := testServer(t)
	conn := APIConnection{URL: "https://api.deepseek.com", Key: key}
	models, err := s.apiCatalog(conn)(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("models: %+v", models)
	config := AIConfig{Primary: "api", API: conn, Models: map[string]string{}, TranslationModels: map[string]string{"api": "deepseek-flash"}}
	passModels(&config, map[string]bool{"deepseek-flash": true})
	service, sessions := s.translationService(config)
	defer sessions.close()
	session := service.connections["api"].Adapter
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	source := "Reading is a conversation with the author. Good readers ask questions as they go."
	batch := translationBatch{Paragraphs: []translationParagraph{{BlockID: "p7-b3", SourceHash: translationHash(source), Source: source}}}
	var rows []TranslationBlock
	decoder := newTranslationJSONL(batch, func(b TranslationBlock) error { rows = append(rows, b); return nil })
	result, err := session.Stream(ctx, GenerateRequest{AIInput{Prompt: translationPrompt(Document{Title: "Live"}, layoutManifest{}, batch, "")}}, func(e ProviderEvent) error {
		return decoder.feed(e.Text)
	})
	if err == nil && strings.TrimSpace(decoder.buffer) != "" {
		err = decoder.line(decoder.buffer)
	}
	if err != nil || len(rows) != 1 {
		t.Fatalf("translation: %v %q", err, result.Text)
	}
	t.Logf("translated: %+v", rows[0].Sentences)
	follow, err := session.Stream(ctx, GenerateRequest{AIInput{Prompt: "Reply with only the blockId of the paragraph you translated in the previous turn."}}, nil)
	if err != nil || !strings.Contains(follow.Text, "p7-b3") {
		t.Fatalf("session lost the previous batch: %v %q", err, follow.Text)
	}
}

// NEOREAD_TEST_DEEPSEEK=1 saves the API agent through HTTP and runs the same
// text and vision checks as Settings.
func TestLiveDeepSeekCapabilityCheck(t *testing.T) {
	key := os.Getenv("DEEPSEEK_API_KEY")
	if os.Getenv("NEOREAD_TEST_DEEPSEEK") != "1" || key == "" {
		t.Skip("set NEOREAD_TEST_DEEPSEEK=1 and DEEPSEEK_API_KEY")
	}
	s := testServer(t)
	config, _ := json.Marshal(AIConfig{Primary: "api", API: APIConnection{URL: "https://api.deepseek.com", Key: key}, Models: map[string]string{}})
	if w := request(t, s, "PUT", "/api/ai/config", strings.NewReader(string(config))); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, model := range []string{"deepseek-flash", "deepseek-v4-pro"} {
		w := request(t, s, "POST", "/api/ai/test/api?model="+model, nil)
		t.Logf("%s: %d %s", model, w.Code, w.Body.String())
	}
	w := request(t, s, "GET", "/api/ai/models?provider=api", nil)
	t.Logf("models: %d %s", w.Code, w.Body.String())
	saved := s.aiConfig()
	if !apiCapable(saved, taskChat, false) || !apiCapable(saved, taskChat, true) {
		t.Fatal("no model passed text and vision")
	}
}

func TestAPIAgentEffortWithoutMetadata(t *testing.T) {
	for _, task := range []modelTask{taskChat, taskTranslation, taskVision} {
		for _, level := range []string{"low", "medium", "high", "max"} {
			t.Run(string(task)+"/"+level, func(t *testing.T) {
				s := testServer(t)
				upstream, calls := fakeAPI(t, func(int, apiCall) string { return "ok" })
				config := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL + "/v1", Key: "sk-test"}, Models: map[string]string{"api": "plain"}, TranslationModels: map[string]string{"api": "plain"}, VisionModels: map[string]string{"api": "plain"}}
				effort := map[string]map[string]string{"api": {"plain": level}}
				config.Efforts, config.TranslationEfforts, config.VisionEfforts = effort, effort, effort
				input := AIInput{Prompt: "hi"}
				if task == taskVision {
					input.Images = [][]byte{{1}}
				}
				if _, err := s.apiAgentAdapter(config, task).Stream(t.Context(), GenerateRequest{input}, nil); err != nil {
					t.Fatal(err)
				}
				want := level
				if want == "max" {
					want = "high"
				}
				if got := calls()[0].Effort; got != want {
					t.Fatalf("sent reasoning_effort %q, want %q", got, want)
				}
			})
		}
	}
}

func TestAPIEffortNearestSupportedTier(t *testing.T) {
	for _, tt := range []struct {
		level     string
		supported []string
		want      string
	}{
		{"medium", []string{"low", "high"}, "high"},
		{"high", []string{"medium", "max"}, "medium"},
		{"low", []string{"max"}, "max"},
		{"max", []string{"low", "medium", "high", "xhigh"}, "xhigh"},
		{"max", []string{"low", "high", "max"}, "max"},
	} {
		if got := apiEffort(tt.level, tt.supported); got != tt.want {
			t.Fatalf("%s/%v: got %s want %s", tt.level, tt.supported, got, tt.want)
		}
	}
}

func TestAPIAgentEffortSurvivesMissingCatalog(t *testing.T) {
	for _, custom := range []bool{false, true} {
		adapter := apiAgentAdapter{
			conn: APIConnection{URL: "https://example.test/v1"},
			text: apiModelChoice{"custom-model", "low"},
			catalog: func(context.Context) ([]AgentModel, error) {
				if custom {
					return []AgentModel{{ID: "custom-model", Custom: true}}, nil
				}
				return nil, errors.New("catalog unavailable")
			},
		}
		req, err := adapter.resolve(t.Context(), false)
		if err != nil || req.Model != "custom-model" || req.Effort != "low" {
			t.Fatalf("%+v %v", req, err)
		}
	}
}

func writeToolReply(w http.ResponseWriter, name, args string) {
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(delta any, finish any) {
		b, _ := json.Marshal(map[string]any{"model": "deepseek-test", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	chunk(map[string]any{"reasoning_content": "opaque-continuation"}, nil)
	chunk(map[string]any{"content": "intermediate tool prose"}, nil)
	split := len(args) / 2
	chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-" + id(), "type": "function", "function": map[string]string{"name": name, "arguments": args[:split]}}}}, nil)
	chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]string{"arguments": args[split:]}}}}, nil)
	chunk(map[string]any{}, "tool_calls")
	fmt.Fprint(w, "data: {\"model\":\"deepseek-test\",\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2,\"total_tokens\":10}}\n\ndata: [DONE]\n\n")
}
func TestDeepSeekDocumentAgentOverHTTP(t *testing.T) {
	s, _ := originalDocumentFixture(t)
	d, _ := s.Store.Document("doc")
	if e := os.WriteFile(s.Store.File(d), []byte("original fixture"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := s.documentCorpus(t.Context(), "doc", nil); e != nil {
		t.Fatal(e)
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"data": []any{map[string]string{"id": "deepseek-test"}}})
			return
		}
		var request struct {
			Messages []toolCompletionMessage `json:"messages"`
			Tools    []any                   `json:"tools"`
			Thinking struct {
				Type string `json:"type"`
			} `json:"thinking"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("bad request")
			w.WriteHeader(400)
			return
		}
		calls++
		if calls > 1 && calls < 7 {
			found := false
			for _, m := range request.Messages {
				if m.Role == "assistant" && len(m.Calls) > 0 {
					found = true
					if m.Reasoning != "opaque-continuation" {
						t.Error("DeepSeek continuation was lost")
					}
				}
			}
			if !found {
				t.Error("tool call IDs/history lost")
			}
		}
		block := func() string {
			var result struct {
				Items []struct {
					Block string `json:"blockId"`
				} `json:"items"`
			}
			if len(request.Messages) == 0 {
				return ""
			}
			text, _ := request.Messages[len(request.Messages)-1].Content.(string)
			json.Unmarshal([]byte(text), &result)
			if len(result.Items) == 0 {
				t.Error("search result unavailable")
				return ""
			}
			return result.Items[0].Block
		}
		switch calls {
		case 1:
			writeToolReply(w, "get_outline", "{}")
		case 2:
			writeToolReply(w, "search_document", `{"query":"COBALT-19"}`)
		case 3:
			writeToolReply(w, "read_passage", jsonText(map[string]any{"blockId": block(), "after": 1}))
		case 4:
			writeToolReply(w, "search_document", `{"query":"37"}`)
		case 5:
			writeToolReply(w, "read_passage", jsonText(map[string]any{"blockId": block()}))
		case 6:
			if len(request.Tools) == 0 {
				t.Error("answer was forced through a separate synthesis request")
			}
			joined := jsonText(request.Messages)
			if !strings.Contains(joined, "COBALT-19") || !strings.Contains(joined, "serial transactions") || !strings.Contains(joined, "37 transactions") {
				t.Error("original evidence lost in same-turn answer")
			}
			writeAPIReply(w, "COBALT-19, serial transactions [S2]. Experiment: 37 [S6].")

		default:
			t.Error("unbounded calls")
			w.WriteHeader(500)
		}
	}))
	defer upstream.Close()
	config := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL, Key: "sk-test"}, Models: map[string]string{"api": "deepseek-test"}, APIModels: []string{"deepseek-test"}, Capabilities: map[string]Capability{}}
	passModels(&config, map[string]bool{"deepseek-test": false})
	cap := config.Capabilities["api:deepseek-test"]
	cap.Tools = "passed"
	cap.ToolSchemaVersion = documentToolSchemaVersion
	config.Capabilities["api:deepseek-test"] = cap
	if e := s.writeAIConfig(config); e != nil {
		t.Fatal(e)
	}
	w := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(`{"provider":"api","prompt":"Compare nonce, condition and experiment","context":""}`))
	if calls != 6 || strings.Contains(w.Body.String(), "event: delta\ndata: {\"text\":\"intermediate tool prose") || strings.Count(w.Body.String(), "event: done") != 1 {
		t.Fatalf("calls=%d stream=%s", calls, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"phase":"indexing"`) || !strings.Contains(w.Body.String(), "正在检索“COBALT-19”") {
		t.Fatal("cached chat rebuilt its index or lost the actual search target")
	}
	var messages []Message
	if e := json.Unmarshal(request(t, s, "GET", "/api/documents/doc/messages", nil).Body.Bytes(), &messages); e != nil {
		t.Fatal(e)
	}
	if len(messages) != 2 || messages[1].Role != "assistant" {
		t.Fatalf("messages: %+v", messages)
	}
	var refs []documentEvidence
	if json.Unmarshal(messages[1].References, &refs) != nil || len(refs) != 2 {
		t.Fatal("citations not persisted")
	}
	sourceURL := "/api/documents/doc/messages/" + messages[1].ID + "/sources/S2"
	if response := request(t, s, "GET", sourceURL, nil); response.Code != 200 {
		t.Fatalf("source: %s", response.Body.String())
	}
	if response := request(t, s, "GET", "/api/documents/doc/messages/"+messages[1].ID+"/sources/S999", nil); response.Code == 200 {
		t.Fatal("forged source navigable")
	}
	var runs int
	s.Store.DB.QueryRow("SELECT count(*) FROM chat_agent_runs").Scan(&runs)
	if runs != 1 {
		t.Fatal("final result not committed once")
	}
	var usageCalls int
	s.Store.DB.QueryRow("SELECT count(*) FROM processing_usage WHERE document_id='doc'").Scan(&usageCalls)
	if usageCalls != calls {
		t.Fatalf("model calls double-counted or missing: usage=%d upstream=%d", usageCalls, calls)
	}
	// A source update invalidates the navigation without deleting historical evidence.
	if e := os.WriteFile(s.Store.File(d), []byte("changed fixture"), 0600); e != nil {
		t.Fatal(e)
	}
	if response := request(t, s, "GET", sourceURL, nil); response.Code != 409 {
		t.Fatal("stale source still navigable")
	}
}
func TestAPIToolProbeAndNonAgentRetrieval(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []toolCompletionMessage `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		calls++
		if calls == 1 {
			writeToolReply(w, "neoread_probe", "{}")
		} else {
			writeAPIReply(w, request.Messages[len(request.Messages)-1].Content.(string))
		}
	}))
	defer upstream.Close()
	status, e := probeAPIDocumentTools(t.Context(), APIConnection{URL: upstream.URL}, "deepseek-test")
	if e != nil || status != "passed" || calls != 2 {
		t.Fatalf("probe %s %v calls=%d", status, e, calls)
	}
	s, a := originalDocumentFixture(t)
	var evidence string
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		json.NewDecoder(r.Body).Decode(&request)
		if _, ok := request["tools"]; ok {
			t.Error("unsupported model received tools")
		}
		evidence = jsonText(request)
		writeAPIReply(w, "Original nonce [S2]")
	}))
	defer plain.Close()
	in := AIInput{Prompt: "COBALT-19", Chat: &chatInput{Question: "COBALT-19", Agent: a}}
	var text strings.Builder
	_, e = streamAPIDocumentChat(t.Context(), APIConnection{URL: plain.URL}, completionRequest{Model: "plain"}, in, func(v ProviderEvent) error { text.WriteString(v.Text); return nil })
	if e != nil || a.Mode != "retrieval" || !strings.Contains(evidence, "serial transactions") || text.Len() == 0 {
		t.Fatalf("retrieval: mode=%s %v", a.Mode, e)
	}
	_ = s
}
func TestToolDecoderRejectsIncompleteAndAmbiguousCalls(t *testing.T) {
	for _, body := range []string{
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"x","arguments":"{}"}}]},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n",
	} {
		if _, e := readToolCompletion(context.Background(), strings.NewReader(body)); e == nil {
			t.Fatal("incomplete/ambiguous tool call accepted")
		}
	}
}

func TestToolDecoderReportsInvalidArguments(t *testing.T) {
	body := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"read_passage","arguments":"{"}}]},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"
	result, err := readToolCompletion(context.Background(), strings.NewReader(body))
	if err != nil || len(result.Calls) != 1 || !result.Calls[0].InvalidArguments {
		t.Fatal("invalid arguments should become a tool error", result, err)
	}
	chunk := jsonText(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "b", "type": "function", "function": map[string]string{"name": "read_passage", "arguments": strings.Repeat("x", 5000)}}}}, "finish_reason": "tool_calls"}}})
	result, err = readToolCompletion(context.Background(), strings.NewReader("data: "+chunk+"\n\ndata: [DONE]\n\n"))
	if err != nil || len(result.Calls) != 1 || !result.Calls[0].InvalidArguments {
		t.Fatal("oversized arguments should become a tool error", result, err)
	}
}

func TestAPICompactionCheckpointBudget(t *testing.T) {
	for _, tc := range []struct {
		name           string
		window, output int
		images         bool
		wantError      bool
	}{
		{"room-for-detailed-handoff", 65536, 8000, false, false},
		{"small-window", 8192, 500, false, false},
		{"fresh-images-keep-their-space", 65536, 500, true, false},
		{"reject-oversized-checkpoint", 262144, 10001, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, a := prepareConversationFixture(t)
			a.contextTokens = tc.window
			a.needsCompaction = true
			question := "KEEP_THIS_QUESTION"
			if tc.images {
				question += strings.Repeat("问", 6500)
				a.canViewPages = true
			}
			messages := []toolCompletionMessage{{Role: "system", Content: "阅读规则"}, {Role: "user", Content: strings.Repeat("旧", tc.window/2)}, {Role: "assistant", Content: "已经确认的结论"}, {Role: "user", Content: question}}
			if tc.images {
				call := completionToolCall{ID: "pages", Type: "function"}
				call.Function.Name = "read_pdf_pages"
				call.Function.Arguments = `{"pages":[1,2,3,4]}`
				messages = append(messages, toolCompletionMessage{Role: "assistant", Calls: []completionToolCall{call}}, toolCompletionMessage{Role: "tool", CallID: call.ID, Content: "四页原图"}, toolCompletionMessage{Role: "user", Content: "工具页面资料", PageImages: []documentPageImage{{Page: 1}, {Page: 2}, {Page: 3}, {Page: 4}}})
			}
			a.conversation = &documentConversation{Messages: messages}
			before := jsonText(messages)
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					Messages []toolCompletionMessage `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					return
				}
				calls++
				prompt := payload.Messages[0].Content.(string)
				match := regexp.MustCompile(`不超过约([0-9]+) tokens`).FindStringSubmatch(prompt)
				if len(match) != 2 {
					t.Error("checkpoint has no explicit budget")
					return
				}
				budget, _ := strconv.Atoi(match[1])
				if budget <= 0 || budget > 10000 {
					t.Error("invalid checkpoint cap", budget)
				}
				if tc.name == "room-for-detailed-handoff" && budget != 10000 {
					t.Error("summary still capped too tightly", budget)
				}
				if (tc.window == 8192 || tc.images) && budget >= 10000 {
					t.Error("budget ignored retained context", budget)
				}
				if strings.Contains(jsonText(payload.Messages), "KEEP_THIS_QUESTION") {
					t.Error("current question entered old-history summary")
				}
				writeConversationReply(w, strings.Repeat("结", tc.output), "")
			}))
			defer upstream.Close()
			next, err := compactAPIConversation(t.Context(), APIConnection{URL: upstream.URL}, completionRequest{Model: "test"}, a, messages)
			if calls == 0 {
				t.Fatal("summary never requested", err)
			}
			if (err != nil) != tc.wantError {
				t.Fatal("unexpected compact result", err)
			}
			if jsonText(messages) != before || jsonText(a.conversation.Messages) != before {
				t.Fatal("input history mutated")
			}
			if tc.wantError {
				if a.conversation.Compactions != 0 {
					t.Fatal("failed checkpoint committed")
				}
				return
			}
			if !strings.Contains(jsonText(next), "KEEP_THIS_QUESTION") || conversationTokens(next) >= a.inputTokenLimit() {
				t.Fatal("current work lost or compacted window over budget")
			}
			if tc.images && len(next[len(next)-1].PageImages) != 4 {
				t.Fatal("fresh images were discarded")
			}
		})
	}
}

func TestToollessEvidenceStaysWithCurrentQuestion(t *testing.T) {
	s, _ := prepareConversationFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"data": []any{map[string]string{"id": "test"}}})
			return
		}
		var p struct {
			Messages []toolCompletionMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		last := p.Messages[len(p.Messages)-1]
		text, _ := last.Content.(string)
		if last.Role != "user" || !strings.Contains(text, "KEEP_THIS_QUESTION") || !strings.Contains(text, "本轮补充原文") {
			t.Error("source envelope displaced the current question", text)
		}
		writeConversationReply(w, "已回答", "")
	}))
	defer upstream.Close()
	configureDocumentAPI(t, s, upstream.URL)
	c := s.aiConfig()
	cap := c.Capabilities["api:test"]
	cap.Tools = "unsupported"
	c.Capabilities["api:test"] = cap
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	if out := conversationRequest(t, s, "api", "KEEP_THIS_QUESTION cobalt"); !strings.Contains(out.Body.String(), "event: done") {
		t.Fatal(out.Body.String())
	}
}

func TestDocumentChatChecksMissingToolCapabilityOnce(t *testing.T) {
	s, _ := pageImageFixture(t, true)
	probes, reads := 0, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"data": []any{map[string]string{"id": "test"}}})
			return
		}
		var p struct {
			Messages []toolCompletionMessage `json:"messages"`
			Tools    []any                   `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		if strings.Contains(jsonText(p.Messages), "Call neoread_probe once") {
			if p.Messages[len(p.Messages)-1].Role == "tool" {
				writeConversationReply(w, p.Messages[len(p.Messages)-1].Content.(string), "")
				return
			}
			probes++
			writeToolReply(w, "neoread_probe", `{}`)
			return
		}
		if !strings.Contains(jsonText(p.Tools), "read_pdf_pages") {
			writeConversationReply(w, "NO_TOOLS", "")
			return
		}
		if p.Messages[len(p.Messages)-1].Content == "再解释一次" {
			writeConversationReply(w, "说明 [S2]", "")
			return
		}
		if len(p.Messages[len(p.Messages)-1].PageImages) == 0 && !strings.Contains(jsonText(p.Messages), "image_url") {
			reads++
			writeToolReply(w, "read_pdf_pages", `{"pages":[2]}`)
			return
		}
		writeConversationReply(w, "第二页图像说明 [S2]", "")
	}))
	defer upstream.Close()
	c := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL}, Models: map[string]string{"api": "test"}, APIModels: []string{"test"}, Capabilities: map[string]Capability{}}
	passModels(&c, map[string]bool{"test": true}) // Legacy configuration: text/vision passed, no tool result.
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		out := conversationRequest(t, s, "api", "第二页的图讲的是什么")
		if !strings.Contains(out.Body.String(), "event: done") || strings.Contains(out.Body.String(), "NO_TOOLS") {
			t.Fatalf("document chat silently ran without tools: %s", out.Body.String())
		}
	}
	if probes != 1 || reads != 1 {
		t.Fatal("missing probe cache or visual read", probes, reads)
	}
	if c = s.aiConfig(); c.Capabilities["api:test"].Tools != "passed" {
		t.Fatal("tool check not persisted")
	}
}

func TestMultimodalToolRequestDoesNotUseTextOnlyBodyLimit(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; writeConversationReply(w, "ok", "") }))
	defer upstream.Close()
	request := completionRequest{Model: "vision", Vision: true, PageImageData: func(documentPageImage) ([]byte, error) { return []byte(strings.Repeat("x", 4<<20)), nil }}
	messages := []toolCompletionMessage{{Role: "user", Content: "page", PageImages: []documentPageImage{{Page: 1, SourceID: "S1"}}}}
	if _, err := toolCompletion(t.Context(), APIConnection{URL: upstream.URL}, request, messages, nil); err != nil || !called {
		t.Fatal("valid image payload rejected by text-only ceiling", err)
	}
}

func TestLiveDeepSeekNaturalFigureQuestion(t *testing.T) {
	if os.Getenv("NEOREAD_TEST_DEEPSEEK_FIGURE") != "1" {
		t.Skip("opt-in DeepSeek natural figure question with configured API")
	}
	raw, err := os.ReadFile(os.Getenv("NEOREAD_TEST_DOCUMENT_API_CONFIG"))
	if err != nil {
		t.Fatal("NeoRead API config unavailable")
	}
	var saved savedConfig
	if json.Unmarshal(raw, &saved) != nil {
		t.Fatal("invalid config")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEOREAD_NODE", node)
	t.Setenv("NEOREAD_PROCESSOR", os.Getenv("NEOREAD_TEST_PAGE_PROCESSOR"))
	s, _ := pageImageFixture(t, false)
	if library := os.Getenv("NEOREAD_TEST_PAPER_LIBRARY"); library != "" {
		id := os.Getenv("NEOREAD_TEST_PAPER_ID")
		decoded, decodeErr := hex.DecodeString(id)
		if decodeErr != nil || len(decoded) != 16 {
			t.Fatal("invalid test paper id")
		}
		pdf, err := os.ReadFile(filepath.Join(library, "papers", id+".pdf"))
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := os.ReadFile(filepath.Join(library, "cache", id, "analysis", "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		d, _ := s.Store.Document("doc")
		if err = os.WriteFile(s.Store.File(d), pdf, 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(s.analysisDir("doc"), "manifest.json"), manifest, 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := AIConfig{Primary: "api", API: saved.Config.API, Models: map[string]string{"api": "deepseek-flash"}, APIModels: []string{"deepseek-flash"}, Capabilities: map[string]Capability{}}
	passModels(&c, map[string]bool{"deepseek-flash": true}) // No prefilled tools result.
	if err = s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	out := conversationRequest(t, s, "api", "第二页的图讲的是什么")
	if !strings.Contains(out.Body.String(), "event: done") {
		t.Fatal(out.Body.String())
	}
	if !strings.Contains(out.Body.String(), "event: reasoning") {
		t.Fatal("DeepSeek thinking was not streamed")
	}
	history, _ := s.Store.chatHistory("doc")
	answer := history[len(history)-1]
	if answer.Reasoning == "" {
		t.Fatal("thinking was not persisted")
	}
	var runRaw string
	_ = s.Store.DB.QueryRow("SELECT body FROM chat_agent_runs WHERE json_extract(body,'$.messageId')=?", answer.ID).Scan(&runRaw)
	var run struct {
		Mode     string              `json:"mode"`
		Tools    []documentToolTrace `json:"tools"`
		Evidence []documentEvidence  `json:"evidence"`
	}
	_ = json.Unmarshal([]byte(runRaw), &run)
	viewed := false
	for _, e := range run.Evidence {
		if e.PageImage != nil && e.PageImage.Page == 2 {
			viewed = true
		}
	}
	if run.Mode != "agent" || !viewed {
		t.Fatal("natural figure question did not trigger image read", run.Mode, jsonText(run.Tools), answer.Content)
	}
	var names []string
	for _, call := range run.Tools {
		names = append(names, call.Tool)
	}
	t.Logf("mode=%s tools=%v reasoningBytes=%d citations=%s", run.Mode, names, len(answer.Reasoning), answer.References)
	t.Log("answer:", answer.Content)
}
