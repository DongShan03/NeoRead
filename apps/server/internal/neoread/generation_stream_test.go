package neoread

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type adapterFunc func(context.Context, GenerateRequest, func(ProviderEvent) error) (GenerateResult, error)

func (f adapterFunc) Stream(c context.Context, r GenerateRequest, e func(ProviderEvent) error) (GenerateResult, error) {
	return f(c, r, e)
}
func TestGenerationAttemptBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		returned  string
		reason    string
		wantError bool
	}{
		{"match", "a", "stop", false}, {"rewrite", "b", "stop", true}, {"no-terminal", "a", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var late func(ProviderEvent) error
			var text string
			a := adapterFunc(func(ctx context.Context, req GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
				late = emit
				_ = emit(ProviderEvent{Text: "a"})
				return GenerateResult{Text: tc.returned, FinishReason: tc.reason}, nil
			})
			_, err := runAttempt(t.Context(), a, AIInput{}, func(e ProviderEvent) error { text += e.Text; return nil })
			if (err != nil) != tc.wantError {
				t.Fatal(err)
			}
			if late(ProviderEvent{Text: "late"}) == nil || text != "a" {
				t.Fatal("late event escaped")
			}
		})
	}
	t.Run("consumer-error", func(t *testing.T) {
		failure := errors.New("write failed")
		calls := 0
		a := adapterFunc(func(ctx context.Context, req GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
			_ = emit(ProviderEvent{Text: "a"})
			_ = emit(ProviderEvent{Text: "late"})
			if ctx.Err() == nil {
				t.Error("not canceled")
			}
			return GenerateResult{Text: "alate", FinishReason: "stop"}, nil
		})
		_, err := runAttempt(t.Context(), a, AIInput{}, func(e ProviderEvent) error { calls++; return failure })
		if !errors.Is(err, failure) || calls != 1 {
			t.Fatal(calls, err)
		}
	})
}
func TestGenerationFallbackPolicy(t *testing.T) {
	for _, mode := range []string{"before", "after", "draft", "reasoning", "background", "cancel", "timeout", "emit-failure", "busy"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "timeout" {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
			}
			backups := 0
			var visible string
			primary := adapterFunc(func(ctx context.Context, req GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
				if mode == "busy" {
					return GenerateResult{}, generationError(ErrorBusy, "会话仍由 Codex App 占用")
				}
				if mode == "draft" {
					_ = emit(ProviderEvent{Draft: "partial"})
				} else if mode == "reasoning" {
					_ = emit(ProviderEvent{Reasoning: "thinking"})
				} else if mode != "before" {
					_ = emit(ProviderEvent{Text: "partial"})
				}
				if mode == "cancel" {
					cancel()
				}
				return GenerateResult{}, errors.New("upstream failed")
			})
			backup := adapterFunc(func(ctx context.Context, req GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
				backups++
				if err := emit(ProviderEvent{Text: "backup"}); err != nil {
					return GenerateResult{}, err
				}
				return GenerateResult{Text: "backup", FinishReason: "stop"}, nil
			})
			g := &GenerationService{primary: "codex", connections: map[string]generationConnection{"codex": {Adapter: primary, VerifiedText: true}, "text-api": {Adapter: backup, VerifiedText: true}}}
			result, err := g.generate(ctx, AIInput{}, false, mode != "background", func(e ProviderEvent) error {
				if mode == "emit-failure" {
					return errors.New("disconnected")
				}
				if mode != "background" {
					visible += e.Text
				}
				return nil
			})
			want := mode == "before" || mode == "background"
			if want {
				if err != nil || backups != 1 || result.Text != "backup" {
					t.Fatal(result, backups, err)
				}
			} else if err == nil || backups != 0 {
				t.Fatal(result, backups, err)
			}
			if mode == "busy" && errorKind(err) != ErrorBusy {
				t.Fatal("thread ownership error was replaced", err)
			}
			if strings.Contains(visible, "partialbackup") {
				t.Fatal("mixed attempts")
			}
		})
	}
}
func TestAgentCancellationHandshake(t *testing.T) {
	for _, p := range []string{"codex", "kimi"} {
		for _, mode := range []string{"protocol-cancel", "ignore-cancel"} {
			t.Run(p+"/"+mode, func(t *testing.T) {
				ack := fakeAgent(t, p, mode)
				s := testServer(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				text := ""
				start := time.Now()
				_, err := invokeCLI(ctx, s.Store.Root, p, "", AIInput{Prompt: "test"}, func(part string) { text += part; cancel() })
				if !errors.Is(err, context.Canceled) || text != "你好" {
					t.Fatalf("text=%q err=%v", text, err)
				}
				if _, err := os.Stat(ack); err != nil {
					t.Fatal("missing protocol cancel", err)
				}
				if time.Since(start) > 2*time.Second {
					t.Fatal("cancel exceeded grace period")
				}
				entries, err := os.ReadDir(filepath.Join(s.Store.Root, "ai-work"))
				if err != nil || len(entries) != 0 {
					t.Fatal("work directory leak", err)
				}
			})
		}
	}
}
func TestAgentTerminalAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		p, mode string
		ok      bool
	}{{"claude", "late", true}, {"claude", "exit-error", false}, {"kimi", "identity", true}, {"kimi", "version", false}} {
		t.Run(tc.p+"/"+tc.mode, func(t *testing.T) {
			fakeAgent(t, tc.p, tc.mode)
			s := testServer(t)
			var text string
			result, err := invokeCLI(t.Context(), s.Store.Root, tc.p, "", AIInput{Prompt: "test"}, func(p string) { text += p })
			if tc.ok {
				if err != nil || text != "你好，世界" || result != text {
					t.Fatal(result, text, err)
				}
			} else if err == nil {
				t.Fatal("expected failure")
			}
		})
	}
}

func TestChatSaveFailureAndWriteFailure(t *testing.T) {
	for _, mode := range []string{"save", "write", "flush"} {
		t.Run(mode, func(t *testing.T) {
			fakeAgent(t, "claude", "success")
			s, _ := imageFixture(t)
			if mode == "save" {
				// SQLite trigger fails only assistant insertion; user record stays intact.
				_, err := s.Store.DB.Exec(`CREATE TRIGGER reject_assistant BEFORE INSERT ON messages WHEN json_extract(NEW.body,'$.role')='assistant' BEGIN SELECT RAISE(FAIL,'test'); END`)
				if err != nil {
					t.Fatal(err)
				}
				res := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(`{"provider":"claude","prompt":"test","context":""}`))
				if strings.Count(res.Body.String(), "event: error") != 1 || strings.Contains(res.Body.String(), "event: done") {
					t.Fatal(res.Body.String())
				}
				var count int
				if err := s.Store.DB.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil || count != 1 {
					t.Fatal(count, err)
				}
			} else {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				recorder := httptest.NewRecorder()
				out := &chatStreamWriter{w: &brokenWriter{ResponseRecorder: recorder, flush: mode == "flush"}, cancel: cancel}
				if out.send("delta", map[string]string{"text": "test"}) == nil || ctx.Err() == nil {
					t.Fatal("write failure did not cancel")
				}
				if out.send("done", map[string]bool{"ok": true}) == nil {
					t.Fatal("terminal gate open")
				}
			}
		})
	}
}

type brokenWriter struct {
	*httptest.ResponseRecorder
	flush bool
}

func (w *brokenWriter) Write(b []byte) (int, error) {
	if !w.flush {
		return 0, errors.New("broken pipe")
	}
	return w.ResponseRecorder.Write(b)
}
func (w *brokenWriter) FlushError() error { return errors.New("flush failed") }

var _ http.ResponseWriter = (*brokenWriter)(nil)

func TestHTTPDisconnectCancelsGeneration(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "kimi"} {
		t.Run(provider, func(t *testing.T) {
			fakeAgent(t, provider, "cancel")
			s, _ := imageFixture(t)
			c := AIConfig{Primary: provider, Models: map[string]string{}, Capabilities: map[string]Capability{}}
			c.Capabilities[provider] = Capability{Text: true, Fingerprint: configPrint(c, provider)}
			if err := s.writeAIConfig(c); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(s.Handler())
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/documents/doc/chat", strings.NewReader(fmt.Sprintf(`{"provider":%q,"prompt":"test","context":""}`, provider)))
			req.Header.Set("Authorization", "Bearer test-secret")
			res, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			scan := bufio.NewScanner(res.Body)
			saw := false
			for scan.Scan() {
				if scan.Text() == "event: delta" {
					saw = true
					break
				}
			}
			_ = res.Body.Close()
			cancel()
			if !saw {
				t.Fatal("no first delta")
			}
			deadline := time.Now().Add(2 * time.Second)
			for !s.aiMu.TryLock() {
				if time.Now().After(deadline) {
					t.Fatal("interactive lock leaked")
				}
				time.Sleep(10 * time.Millisecond)
			}
			s.aiMu.Unlock()
			var count int
			if err := s.Store.DB.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil || count != 1 {
				t.Fatal("partial answer saved", count, err)
			}
			entries, err := os.ReadDir(filepath.Join(s.Store.Root, "ai-work"))
			if err != nil || len(entries) != 0 {
				t.Fatal("work directory leaked", err)
			}
		})
	}
}

type lostDoneWriter struct{ *httptest.ResponseRecorder }

func (w lostDoneWriter) Write(b []byte) (int, error) {
	if strings.HasPrefix(string(b), "event: done") {
		return 0, errors.New("disconnected after save")
	}
	return w.ResponseRecorder.Write(b)
}
func TestDoneLossDoesNotRollbackSavedAnswer(t *testing.T) {
	fakeAgent(t, "claude", "success")
	s, _ := imageFixture(t)
	req := httptest.NewRequest("POST", "http://127.0.0.1:17840/api/documents/doc/chat", strings.NewReader(`{"provider":"claude","prompt":"test","context":""}`))
	req.Header.Set("Authorization", "Bearer test-secret")
	w := lostDoneWriter{httptest.NewRecorder()}
	s.Handler().ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), "event: done") || strings.Contains(w.Body.String(), "event: error") {
		t.Fatal(w.Body.String())
	}
	var count int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil || count != 2 {
		t.Fatal("saved answer lost", count, err)
	}
}
func TestChatRequiredFields(t *testing.T) {
	s, _ := imageFixture(t)
	for _, body := range []string{`{"prompt":"test","context":""}`, `{"provider":"codex","prompt":"test"}`, `{"provider":"codex","prompt":"test","context":null}`} {
		res := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(body))
		if res.Code != 400 {
			t.Fatal(res.Code, res.Body.String())
		}
	}
}

func TestChatGenerationDeadlineAppliesToAllProviders(t *testing.T) {
	for _, provider := range []string{"api", "codex", "claude", "kimi"} {
		t.Run(provider, func(t *testing.T) {
			adapter := adapterFunc(func(ctx context.Context, _ GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) < 59*time.Minute || time.Until(deadline) > time.Hour {
					t.Errorf("unexpected chat deadline %v", deadline)
				}
				if err := emit(ProviderEvent{Text: "complete"}); err != nil {
					return GenerateResult{}, err
				}
				return GenerateResult{Text: "complete", FinishReason: "stop"}, nil
			})
			service := &GenerationService{primary: provider, connections: map[string]generationConnection{provider: {Adapter: adapter, VerifiedText: true}}}
			_, err := service.generate(context.Background(), AIInput{Chat: &chatInput{}}, false, true, nil)
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUsageLedgerKeepsConcurrentFailuresSnapshotsAndRetries(t *testing.T) {
	s, p := processingFixture(t)
	sink := s.processingUsageSink(p.DocumentID, "settling", "p1-b1")
	call := UsageCall{ID: "first", Provider: "codex", StartedAt: now(), Status: "running", Models: []ModelTokens{{Model: "actual", Tokens: &TokenCounts{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, CachedInputTokens: tokenCount(7), ReasoningOutputTokens: tokenCount(2)}}}}
	if err := sink(call); err != nil {
		t.Fatal(err)
	}
	call.Status = "failed"
	call.FinishedAt = now()
	if err := sink(call); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := UsageCall{ID: fmt.Sprint("retry", i), Provider: "image-api", StartedAt: now(), FinishedAt: now(), Status: "complete", Models: []ModelTokens{{Model: "backup", Tokens: &TokenCounts{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}}}}
			if err := sink(c); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	call.ID = "unknown"
	call.Models = []ModelTokens{{Model: "", Tokens: nil}}
	if err := sink(call); err != nil {
		t.Fatal(err)
	}
	report, err := s.Store.usageReport("doc")
	if err != nil {
		t.Fatal(err)
	}
	if report.Total.TotalTokens != 33 || report.Total.InputTokens != 22 || report.Total.OutputTokens != 11 || (report.Total.CachedInputTokens == nil || *report.Total.CachedInputTokens != 7) || report.FailedCalls != 2 || report.PartialCalls != 1 || report.UnknownCalls != 1 || len(report.Calls) != 8 {
		t.Fatalf("%+v", report)
	}
	// A repeated snapshot must not count the same model's cumulative tokens twice.
	models := mergeModelTokens([]ModelTokens{{Model: "actual"}}, []ModelTokens{{Tokens: &TokenCounts{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}}})
	models = mergeModelTokens(models, []ModelTokens{{Tokens: &TokenCounts{InputTokens: 2, OutputTokens: 4, TotalTokens: 6}}})
	if len(models) != 1 || models[0].Model != "actual" || models[0].Tokens.TotalTokens != 6 {
		t.Fatal(models)
	}
	// Durable through reopen, cascades with document deletion.
	reopened, err := OpenStore(s.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	r, err := reopened.usageReport("doc")
	if err != nil || r.Total.TotalTokens != report.Total.TotalTokens {
		t.Fatal(r, err)
	}
	if _, err = s.Store.DB.Exec("DELETE FROM documents WHERE id='doc'"); err != nil {
		t.Fatal(err)
	}
	var n int
	s.Store.DB.QueryRow("SELECT count(*) FROM processing_usage").Scan(&n)
	if n != 0 {
		t.Fatal(n)
	}
}
func TestUsageRecordsFallbackAndPreservesFailedUsage(t *testing.T) {
	s, _ := processingFixture(t)
	g := &GenerationService{primary: "codex", usageSink: s.processingUsageSink("doc", "settling", "p1-b1"), connections: map[string]generationConnection{
		"codex": {VerifiedText: true, VerifiedVision: true, Adapter: adapterFunc(func(ctx context.Context, req GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
			emit(ProviderEvent{Metrics: []ModelTokens{{Model: "primary", Tokens: &TokenCounts{InputTokens: 4, OutputTokens: 1, TotalTokens: 5}}}})
			return GenerateResult{}, errors.New("upstream")
		})},
		"image-api": {VerifiedText: true, VerifiedVision: true, Adapter: adapterFunc(func(ctx context.Context, req GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
			emit(ProviderEvent{Metrics: []ModelTokens{{Model: "backup", Tokens: &TokenCounts{InputTokens: 8, OutputTokens: 2, TotalTokens: 10}}}})
			emit(ProviderEvent{Text: "answer"})
			return GenerateResult{Text: "answer", FinishReason: "stop"}, nil
		})},
	}}
	result, err := g.Generate(t.Context(), AIInput{Image: []byte("png")}, false, nil)
	if err != nil || !result.Fallback {
		t.Fatal(result, err)
	}
	r, err := s.Store.usageReport("doc")
	if err != nil || len(r.Calls) != 2 || r.Total.TotalTokens != 15 || r.UnknownCalls != 0 || r.PartialCalls != 1 {
		t.Fatal(r, err)
	}
}
func TestUsageUnknownHistoryAndInterruptedAttempt(t *testing.T) {
	s, p := processingFixture(t)
	p.UsageTracked = false
	s.Store.saveProcessing(p)
	sink := s.processingUsageSink("doc", "translating", "p1-b1")
	sink(UsageCall{ID: "open", Provider: "kimi", StartedAt: now(), Status: "running", Models: []ModelTokens{{Model: "kimi", Tokens: nil}}})
	if err := s.Store.recoverUsage(); err != nil {
		t.Fatal(err)
	}
	r, err := s.Store.usageReport("doc")
	if err != nil || r.HistoryComplete || r.UnknownCalls != 1 || r.Calls[0].Status != "interrupted" {
		t.Fatal(r, err)
	}
	s.Token = "test-secret"
	response := request(t, s, "GET", "/api/documents/doc/processing-usage", nil)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
}
func TestAPIUsageOnlyFrameAfterFinishIsNotText(t *testing.T) {
	wire := `data: {"model":"actual-api","choices":[{"index":0,"delta":{"content":"answer"}}]}` + "\n\n" + `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" + `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13,"prompt_tokens_details":{"cached_tokens":7},"completion_tokens_details":{"reasoning_tokens":1}}}` + "\n\n" + "data: [DONE]\n\n"
	var models []ModelTokens
	var text string
	result, err := readCompletionStream(t.Context(), strings.NewReader(wire), func(event ProviderEvent) error {
		models = mergeModelTokens(models, event.Metrics)
		text += event.Text
		return nil
	})
	if err != nil || result.Text != "answer" || text != "answer" || len(models) != 1 || models[0].Model != "actual-api" || models[0].Tokens.TotalTokens != 13 {
		t.Fatal(result, models, err)
	}
	if completionTokens(&completionUsage{}) != nil {
		t.Fatal("missing counts became zero")
	}
}
func TestClaudeUsageNormalizesCacheAndMultipleModels(t *testing.T) {
	var metrics []ModelTokens
	reportClaudeMetrics([]byte(`{"type":"result","modelUsage":{"model-a":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":7,"cacheCreationInputTokens":3},"model-b":{"inputTokens":2,"outputTokens":1}}}`), []func([]ModelTokens){func(m []ModelTokens) { metrics = m }})
	if len(metrics) != 2 || metrics[0].Tokens.TotalTokens != 25 || metrics[0].Tokens.InputTokens != 20 || metrics[1].Tokens.TotalTokens != 3 {
		t.Fatal(metrics)
	}
}
func TestCodexUsageUsesThreadTotalAndFiltersOtherThreads(t *testing.T) {
	// Real adapter with the existing CLI protocol fixture, no live credentials.
	fakeAgent(t, "codex", "usage")
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "ai-work"), 0700)
	var models []ModelTokens
	text, err := invokeCLI(t.Context(), root, "codex", "", AIInput{Prompt: "test"}, nil, func(m []ModelTokens) { models = mergeModelTokens(models, m) })
	if err != nil || text != "你好，世界" || len(models) != 1 || models[0].Model != "actual-codex" || models[0].Tokens == nil || models[0].Tokens.TotalTokens != 30 {
		b, _ := json.Marshal(models)
		t.Fatalf("%s %s %v", text, b, err)
	}
}

func TestCodexMissingCountersRemainUnknown(t *testing.T) {
	for _, raw := range []string{`{}`, `{"inputTokens":null,"outputTokens":0,"totalTokens":0}`, `{"inputTokens":-1,"outputTokens":1,"totalTokens":0}`} {
		if codexTokens(json.RawMessage(raw)) != nil {
			t.Fatal(raw)
		}
	}
	metrics := []ModelTokens{}
	reportClaudeMetrics([]byte(`{"type":"result","modelUsage":{"model":{}}}`), []func([]ModelTokens){func(m []ModelTokens) { metrics = m }})
	if len(metrics) != 1 || metrics[0].Tokens != nil {
		t.Fatal(metrics)
	}
}
func TestUsageStorageFailureDoesNotTriggerFallback(t *testing.T) {
	attempts := 0
	g := &GenerationService{primary: "codex", usageSink: func(UsageCall) error { return errors.New("storage unavailable") }, connections: map[string]generationConnection{}}
	for _, provider := range []string{"codex", "text-api"} {
		g.connections[provider] = generationConnection{VerifiedText: true, Adapter: adapterFunc(func(context.Context, GenerateRequest, func(ProviderEvent) error) (GenerateResult, error) {
			attempts++
			return GenerateResult{}, nil
		})}
	}
	_, err := g.Generate(t.Context(), AIInput{}, false, nil)
	if errorKind(err) != ErrorSave || attempts != 0 {
		t.Fatal(err, attempts)
	}
}

func TestChatUsageRecordsTextAndImageAndSeparatesProcessing(t *testing.T) {
	fakeAgent(t, "codex", "usage")
	s, _ := imageFixture(t)
	config := AIConfig{Primary: "codex", Models: map[string]string{}, Capabilities: map[string]Capability{}}
	config.Capabilities["codex"] = Capability{Text: true, Vision: true, Fingerprint: configPrint(config, "codex")}
	if err := s.writeAIConfig(config); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"provider":"codex","prompt":"explain","context":""}`, `{"provider":"codex","prompt":"describe image","context":"","attachments":["p1-b1"]}`} {
		res := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(body))
		if !strings.Contains(res.Body.String(), "event: done") {
			t.Fatal(res.Body.String())
		}
	}
	r, err := s.Store.scopedUsageReport("doc", true)
	if err != nil || !r.HistoryComplete || len(r.Calls) != 2 || r.Total.TotalTokens != 60 || r.Calls[0].Target == r.Calls[1].Target || r.Calls[0].Models[0].Model != "actual-codex" {
		t.Fatal(r, err)
	}
	processing, err := s.Store.usageReport("doc")
	if err != nil || len(processing.Calls) != 0 || processing.Total.TotalTokens != 0 {
		t.Fatal(processing, err)
	}
	// EPUB chat reporting needs no PDF processing task.
	if _, err = s.Store.DB.Exec("DELETE FROM document_processing WHERE document_id='doc'"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.DB.Exec("UPDATE documents SET type='epub' WHERE id='doc'"); err != nil {
		t.Fatal(err)
	}
	res := request(t, s, "GET", "/api/documents/doc/chat-usage", nil)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	var apiReport UsageReport
	if err = json.Unmarshal(res.Body.Bytes(), &apiReport); err != nil || apiReport.Total.TotalTokens != 60 {
		t.Fatal(apiReport, err)
	}
	// Clearing conversation text must not erase usage that already occurred.
	s.Store.DB.Exec("DELETE FROM messages WHERE document_id='doc'")
	reopened, err := OpenStore(s.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	r, err = reopened.scopedUsageReport("doc", true)
	if err != nil || !r.HistoryComplete || r.Total.TotalTokens != 60 {
		t.Fatal(r, err)
	}
	s.Store.DB.Exec("DELETE FROM documents WHERE id='doc'")
	var n int
	s.Store.DB.QueryRow("SELECT count(*) FROM processing_usage").Scan(&n)
	if n != 0 {
		t.Fatal(n)
	}
	s.Store.DB.QueryRow("SELECT count(*) FROM chat_usage_coverage").Scan(&n)
	if n != 0 {
		t.Fatal(n)
	}
}
func TestChatUsageMigrationAndDocumentIsolation(t *testing.T) {
	s, p := processingFixture(t)
	if err := s.Store.saveMessage(Message{DocumentID: p.DocumentID, Role: "assistant", Content: "old"}); err != nil {
		t.Fatal(err)
	}
	// Simulate a library from before chat usage was introduced.
	s.Store.DB.Exec("DELETE FROM chat_usage_coverage")
	reopened, err := OpenStore(s.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	r, err := reopened.scopedUsageReport(p.DocumentID, true)
	if err != nil || r.HistoryComplete {
		t.Fatal(r, err)
	}
	stamp := now()
	if _, err = s.Store.DB.Exec("INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at) VALUES('other','epub','other','',0,?,?)", stamp, stamp); err != nil {
		t.Fatal(err)
	}
	sink := s.processingUsageSink(p.DocumentID, "chat", "request")
	if err = sink(UsageCall{ID: "failed-chat", Status: "failed", StartedAt: stamp, FinishedAt: stamp, Models: []ModelTokens{{Model: "actual", Tokens: &TokenCounts{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}}}}); err != nil {
		t.Fatal(err)
	}
	r, err = s.Store.scopedUsageReport(p.DocumentID, true)
	if err != nil || r.Total.TotalTokens != 3 || r.FailedCalls != 1 || r.PartialCalls != 1 || r.HistoryComplete {
		t.Fatal(r, err)
	}
	other, err := s.Store.scopedUsageReport("other", true)
	if err != nil || len(other.Calls) != 0 || other.Total.TotalTokens != 0 || !other.HistoryComplete {
		t.Fatal(other, err)
	}
}

// Run the test binary as the CLI so protocol tests need no installed agents,
// credentials, network, or additional scripting runtime.
func fakeAgent(t *testing.T, provider, mode string) string {
	t.Helper()
	dir := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := provider
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(dir, name)
	// Windows keeps the running test executable locked, including its hard links.
	// Copy there so TempDir cleanup can remove the helper after the child exits.
	if runtime.GOOS == "windows" || os.Link(binary, target) != nil {
		data, err := os.ReadFile(binary)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NEOREAD_AGENT_HELPER", provider)
	t.Setenv("NEOREAD_AGENT_MODE", mode)
	if provider == "claude" {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	}
	ack := filepath.Join(dir, "received")
	t.Setenv("NEOREAD_AGENT_ACK", ack)
	return ack
}

func runAgentProcess() {
	provider := os.Getenv("NEOREAD_AGENT_HELPER")
	if provider == "" {
		return
	}
	mode := os.Getenv("NEOREAD_AGENT_MODE")
	defer os.Exit(0)
	emit := func(v any) { _ = json.NewEncoder(os.Stdout).Encode(v) }
	gate := func() {
		if mode != "gated" && mode != "cancel" {
			return
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(os.Getenv("NEOREAD_AGENT_ACK")); err == nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
		os.Exit(12) // No callback before completion: buffering regression.
	}
	args := strings.Join(os.Args, " ")
	if strings.Contains(args, "login status") {
		return
	}
	if strings.Contains(args, "auth status") {
		if mode == "user-settings" {
			emit(map[string]bool{"loggedIn": false})
			os.Exit(1)
		}
		emit(map[string]bool{"loggedIn": true})
		return
	}
	if provider == "claude" {
		if mode == "user-settings" && !strings.Contains(args, "--setting-sources user") {
			os.Exit(1)
		}
		if path := os.Getenv("NEOREAD_SYSTEM_CAPTURE"); path != "" {
			for i, arg := range os.Args {
				if arg == "--append-system-prompt" && i+1 < len(os.Args) {
					_ = os.WriteFile(path, []byte(os.Args[i+1]), 0600)
				}
			}
		}
		if !strings.Contains(args, "--include-partial-messages") {
			os.Exit(10)
		}
		reader := bufio.NewReader(os.Stdin)
		first, _ := reader.ReadString('\n')
		if strings.Contains(first, `"type":"control_request"`) {
			if mode == "user-settings" {
				os.Exit(1)
			}
			emit(map[string]any{"type": "control_response", "response": map[string]any{"request_id": "1", "subtype": "success", "response": map[string]any{"models": []any{map[string]any{"value": "test", "resolvedModel": "test-model", "displayName": "Test"}}}}})
			return
		}
		rest, _ := io.ReadAll(reader)
		if runFakeClaudeDocument(mode, first+string(rest), emit, gate) {
			return
		}
		if path := os.Getenv("NEOREAD_AGENT_CAPTURE"); path != "" {
			_ = os.WriteFile(path, []byte(args), 0600)
		}
		if mode == "fail-before" {
			os.Exit(1)
		}
		emit(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]string{"type": "thinking_delta", "thinking": "private"}}})
		part := func(text string) {
			emit(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]string{"type": "text_delta", "text": text}}})
		}
		part("你好")
		gate()
		if mode == "fail-after" {
			emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true})
			return
		}
		if mode == "truncated" {
			return
		}
		part("，世界")
		emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]string{"type": "text", "text": "你好，世界"}}}})
		emit(map[string]any{"type": "result", "subtype": "success", "result": "你好，世界"})
		if mode == "late" {
			part("late")
			emit(map[string]any{"type": "result", "subtype": "success"})
		}
		if mode == "exit-error" {
			os.Exit(1)
		}
		return
	}
	if provider == "kimi" {
		if strings.HasPrefix(mode, "document-") {
			runFakeKimiDocument(mode, emit)
			return
		}
		if path := os.Getenv("NEOREAD_SYSTEM_CAPTURE"); path != "" {
			for i, arg := range os.Args {
				if arg == "--agent-file" && i+1 < len(os.Args) {
					data, _ := os.ReadFile(os.Args[i+1])
					_ = os.WriteFile(path, data, 0600)
				}
			}
		}
	}
	decoder := json.NewDecoder(os.Stdin)
	turnNumber := 0
	for {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if decoder.Decode(&req) != nil {
			return
		}
		respond := func(v any) { emit(map[string]any{"id": req.ID, "result": v}) }
		if provider == "codex" {
			if !strings.Contains(args, "app-server") {
				os.Exit(11)
			}
			switch req.Method {
			case "initialize":
				respond(map[string]string{"userAgent": "test"})
			case "config/read":
				respond(map[string]any{"config": map[string]any{"mcp_servers": map[string]any{}, "features": map[string]bool{}, "web_search": "disabled"}})
			case "initialized":
			case "model/list":
				respond(map[string]any{"data": []any{map[string]any{"model": "test-model", "displayName": "Test", "isDefault": true}}})
			case "thread/start":
				var params map[string]any
				_ = json.Unmarshal(req.Params, &params)
				if path := os.Getenv("NEOREAD_SYSTEM_CAPTURE"); path != "" {
					instructions, _ := params["baseInstructions"].(string)
					_ = os.WriteFile(path, []byte(instructions), 0600)
				}
				if params["ephemeral"] != true || params["sandbox"] != "read-only" || params["approvalPolicy"] != "never" {
					os.Exit(13)
				}
				if path := os.Getenv("NEOREAD_AGENT_THREAD_CAPTURE"); path != "" {
					_ = os.WriteFile(path, req.Params, 0600)
				}
				respond(map[string]any{"thread": map[string]string{"id": "thread"}, "model": "actual-codex"})
			case "turn/start":
				turnNumber++
				currentTurn := fmt.Sprintf("turn-%d", turnNumber)
				if path := os.Getenv("NEOREAD_AGENT_CAPTURE"); path != "" {
					_ = os.WriteFile(path, req.Params, 0600)
				}
				if mode != "interleaved" {
					respond(map[string]any{"turn": map[string]string{"id": currentTurn, "status": "inProgress"}})
				}
				notify := func(method string, params map[string]any) {
					params["threadId"] = "thread"
					params["turnId"] = currentTurn
					emit(map[string]any{"method": method, "params": params})
				}
				if mode == "fail-before" {
					notify("turn/completed", map[string]any{"turn": map[string]string{"id": currentTurn, "status": "failed"}})
					continue
				}
				if mode == "protocol" {
					emit(map[string]any{"id": "approval", "method": "item/commandExecution/requestApproval", "params": map[string]string{"threadId": "thread", "turnId": currentTurn}})
					var denied struct {
						ID    string          `json:"id"`
						Error json.RawMessage `json:"error"`
					}
					if decoder.Decode(&denied) != nil || denied.ID != "approval" || len(denied.Error) == 0 {
						os.Exit(14)
					}
					emit(map[string]any{"method": "item/agentMessage/delta", "params": map[string]string{"threadId": "other", "turnId": currentTurn, "itemId": "other", "delta": "private"}})
					emit(map[string]any{"method": "item/agentMessage/delta", "params": map[string]string{"threadId": "thread", "turnId": "other", "itemId": "other", "delta": "private"}})
				}
				if mode == "usage" {
					emit(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": "other", "tokenUsage": map[string]any{"total": TokenCounts{TotalTokens: 999}}}})
					for _, n := range []int64{15, 30, 30} {
						emit(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": "thread", "tokenUsage": map[string]any{"total": TokenCounts{InputTokens: n - 5, OutputTokens: 5, TotalTokens: n}, "last": TokenCounts{TotalTokens: 2}}}})
					}
				}
				notify("item/reasoning/textDelta", map[string]any{"delta": "private"})
				notify("item/commandExecution/outputDelta", map[string]any{"delta": "tool output"})
				notify("item/started", map[string]any{"item": map[string]string{"id": "answer", "type": "agentMessage", "phase": "final_answer"}})
				notify("item/agentMessage/delta", map[string]any{"itemId": "answer", "delta": "你好"})
				if mode == "interleaved" {
					respond(map[string]any{"turn": map[string]string{"id": currentTurn, "status": "inProgress"}})
				}
				if mode == "protocol-cancel" || mode == "ignore-cancel" {
					var interrupt struct {
						ID     json.RawMessage `json:"id"`
						Method string          `json:"method"`
					}
					if decoder.Decode(&interrupt) != nil || interrupt.Method != "turn/interrupt" {
						os.Exit(20)
					}
					_ = os.WriteFile(os.Getenv("NEOREAD_AGENT_ACK"), []byte("interrupt"), 0600)
					if mode == "ignore-cancel" {
						time.Sleep(10 * time.Second)
						return
					}
					notify("item/agentMessage/delta", map[string]any{"itemId": "answer", "delta": "late"})
					emit(map[string]any{"id": interrupt.ID, "result": map[string]any{}})
					notify("turn/completed", map[string]any{"turn": map[string]string{"id": currentTurn, "status": "interrupted"}})
					continue
				}
				gate()
				if mode == "truncated" {
					return
				}
				if mode == "fail-after" {
					notify("turn/completed", map[string]any{"turn": map[string]string{"id": currentTurn, "status": "failed"}})
					continue
				}
				if mode != "suffix" {
					notify("item/agentMessage/delta", map[string]any{"itemId": "answer", "delta": "，世界"})
				}
				final := "你好，世界"
				if mode == "rewrite" {
					final = "different answer"
				}
				notify("item/completed", map[string]any{"item": map[string]string{"id": "answer", "type": "agentMessage", "text": final}})
				notify("turn/completed", map[string]any{"turn": map[string]string{"id": currentTurn, "status": "completed"}})
			}
		} else {
			switch req.Method {
			case "initialize":
				version := 1
				if mode == "version" {
					version = 2
				}
				respond(map[string]any{"protocolVersion": version, "agentCapabilities": map[string]any{"promptCapabilities": map[string]bool{"image": true}}})
			case "session/new":
				respond(map[string]any{"sessionId": "session", "configOptions": []any{map[string]any{"id": "model", "currentValue": "test-model", "options": []any{map[string]string{"value": "test-model", "name": "Test"}}}}})
			case "session/set_config_option":
				if path := os.Getenv("NEOREAD_AGENT_CAPTURE"); path != "" {
					_ = os.WriteFile(path, req.Params, 0600)
				}
				respond(map[string]any{})
			case "session/prompt":
				if mode == "fail-before" {
					emit(map[string]any{"id": req.ID, "error": map[string]any{"code": -1, "message": "test failure"}})
					continue
				}
				part := func(kind, text string) {
					emit(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": "session", "update": map[string]any{"sessionUpdate": kind, "content": map[string]string{"type": "text", "text": text}}}})
				}
				if mode == "identity" {
					emit(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": "other", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "private"}}}})
				}
				part("agent_thought_chunk", "private")
				part("agent_message_chunk", "你好")
				if mode == "protocol-cancel" || mode == "ignore-cancel" {
					var interrupt struct {
						ID     json.RawMessage `json:"id"`
						Method string          `json:"method"`
					}
					if decoder.Decode(&interrupt) != nil || interrupt.Method != "session/cancel" || len(interrupt.ID) > 0 {
						os.Exit(21)
					}
					_ = os.WriteFile(os.Getenv("NEOREAD_AGENT_ACK"), []byte("cancel"), 0600)
					if mode == "ignore-cancel" {
						time.Sleep(10 * time.Second)
						return
					}
					part("agent_message_chunk", "late")
					respond(map[string]string{"stopReason": "cancelled"})
					continue
				}
				gate()
				if mode == "truncated" {
					return
				}
				if mode == "fail-after" {
					respond(map[string]string{"stopReason": "cancelled"})
					continue
				}
				part("agent_message_chunk", "，世界")
				respond(map[string]string{"stopReason": "end_turn"})
			}
		}
	}
}

func TestAgentStreamingBeforeCompletion(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "kimi"} {
		t.Run(provider, func(t *testing.T) {
			ack := fakeAgent(t, provider, "gated")
			s := testServer(t)
			c := AIConfig{Primary: provider, Models: map[string]string{}, Capabilities: map[string]Capability{}}
			c.Capabilities[provider] = Capability{Text: true, Vision: true, Fingerprint: configPrint(c, provider)}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var parts []string
			result, err := s.generateWithConfig(ctx, c, AIInput{Prompt: "test", Image: []byte{1}}, func(part string) {
				parts = append(parts, part)
				if err := os.WriteFile(ack, []byte("received"), 0600); err != nil {
					t.Error(err)
				}
			}, nil)
			if err != nil || result.Text != "你好，世界" || len(parts) != 2 || strings.Join(parts, "") != result.Text {
				t.Fatalf("expected two deltas before completion, got parts=%q result=%+v error=%v", parts, result, err)
			}
		})
	}
}

func TestAgentPartialFailureDoesNotFallback(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "kimi"} {
		t.Run(provider, func(t *testing.T) {
			fakeAgent(t, provider, "fail-after")
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("fallback called after visible text")
				writeAPIReply(w, "fallback")
			}))
			defer api.Close()
			s := testServer(t)
			c := AIConfig{Primary: provider, Models: map[string]string{}, TextAPI: APIConnection{URL: api.URL, Model: "test"}, Capabilities: map[string]Capability{}}
			for _, p := range []string{provider, "text-api"} {
				c.Capabilities[p] = Capability{Text: true, Fingerprint: configPrint(c, p)}
			}
			var text string
			_, err := s.generateWithConfig(t.Context(), c, AIInput{Prompt: "test"}, func(part string) { text += part }, func(string) { t.Error("fallback notification after visible text") })
			if err == nil || text != "你好" {
				t.Fatalf("partial failure: text=%q error=%v", text, err)
			}
		})
	}
}

func TestCodexProtocolBoundaries(t *testing.T) {
	for _, mode := range []string{"protocol", "interleaved", "suffix", "rewrite"} {
		t.Run(mode, func(t *testing.T) {
			fakeAgent(t, "codex", mode)
			s := testServer(t)
			capture := filepath.Join(t.TempDir(), "input.json")
			threadCapture := filepath.Join(t.TempDir(), "thread.json")
			t.Setenv("NEOREAD_AGENT_CAPTURE", capture)
			t.Setenv("NEOREAD_AGENT_THREAD_CAPTURE", threadCapture)
			var parts []string
			answer, err := invokeCLI(t.Context(), s.Store.Root, "codex", "selected-model", AIInput{Prompt: "test", Images: [][]byte{{1}, {2}}}, func(part string) { parts = append(parts, part) })
			if mode == "rewrite" {
				if err == nil {
					t.Fatal("inconsistent final text accepted")
				}
				return
			}
			if err != nil || answer != "你好，世界" || len(parts) != 2 || strings.Join(parts, "") != answer {
				t.Fatalf("parts=%q answer=%q error=%v", parts, answer, err)
			}
			b, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			var turn struct {
				Input []struct {
					Type string `json:"type"`
					URL  string `json:"url"`
				} `json:"input"`
			}
			if json.Unmarshal(b, &turn) != nil || len(turn.Input) != 3 || turn.Input[1].URL != imageData([]byte{1}) || turn.Input[2].URL != imageData([]byte{2}) {
				t.Fatalf("image inputs missing: %s", b)
			}
			b, err = os.ReadFile(threadCapture)
			if err != nil {
				t.Fatal(err)
			}
			var thread struct {
				Model string `json:"model"`
				CWD   string `json:"cwd"`
			}
			if json.Unmarshal(b, &thread) != nil || thread.Model != "selected-model" || !strings.HasPrefix(thread.CWD, filepath.Join(s.Store.Root, "ai-work")) {
				t.Fatalf("thread configuration: %s", b)
			}
		})
	}
}

func TestAgentFailureBeforeTextCanFallback(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			fakeAgent(t, provider, "fail-before")
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeAPIReply(w, "fallback")
			}))
			defer api.Close()
			s := testServer(t)
			config := AIConfig{Primary: provider, Models: map[string]string{}, TextAPI: APIConnection{URL: api.URL, Model: "test"}, Capabilities: map[string]Capability{}}
			for _, p := range []string{provider, "text-api"} {
				config.Capabilities[p] = Capability{Text: true, Fingerprint: configPrint(config, p)}
			}
			var text string
			notified := false
			result, err := s.generateWithConfig(t.Context(), config, AIInput{Prompt: "test"}, func(part string) { text += part }, func(string) { notified = true })
			if err != nil || text != "fallback" || !notified || !result.Fallback {
				t.Fatalf("fallback: %+v %q %v", result, text, err)
			}
		})
	}
}

func TestAgentTruncationAndCancellation(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "kimi"} {
		for _, mode := range []string{"truncated", "cancel", "cancel-buffered"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				fakeAgent(t, provider, mode)
				s := testServer(t)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				chunks := 0
				_, err := invokeCLI(ctx, s.Store.Root, provider, "", AIInput{Prompt: "test"}, func(string) {
					chunks++
					if strings.HasPrefix(mode, "cancel") {
						cancel()
					}
				})
				if err == nil {
					t.Fatal("incomplete answer accepted")
				}
				if strings.HasPrefix(mode, "cancel") && (err != context.Canceled || chunks != 1) {
					t.Fatalf("cancellation: %v", err)
				}
				files, _ := os.ReadDir(filepath.Join(s.Store.Root, "ai-work"))
				if len(files) != 0 {
					t.Fatal("temporary work directory leaked")
				}
			})
		}
	}
}

func TestChatStreamsOverHTTP(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "kimi"} {
		for _, withImage := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/image=%t", provider, withImage), func(t *testing.T) {
				ack := fakeAgent(t, provider, "gated")
				s, _ := imageFixture(t)
				config := AIConfig{Primary: provider, Models: map[string]string{}, Capabilities: map[string]Capability{}}
				config.Capabilities[provider] = Capability{Text: true, Vision: true, Fingerprint: configPrint(config, provider)}
				if err := s.writeAIConfig(config); err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(s.Handler())
				defer server.Close()
				body := map[string]any{"provider": provider, "prompt": "explain", "context": "source"}
				if withImage {
					body["attachments"] = []string{"p1-b1"}
				}
				b, _ := json.Marshal(body)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/documents/doc/chat", strings.NewReader(string(b)))
				req.Header.Set("Authorization", "Bearer test-secret")
				res, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				if res.StatusCode != 200 {
					t.Fatalf("status %d", res.StatusCode)
				}
				scanner := bufio.NewScanner(res.Body)
				event, text, done, chunks := "", "", false, 0
				for scanner.Scan() {
					line := scanner.Text()
					if strings.HasPrefix(line, "event: ") {
						event = strings.TrimPrefix(line, "event: ")
					}
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					if event == "error" {
						t.Fatal(line)
					}
					if event == "done" {
						done = true
					}
					if event != "delta" {
						continue
					}
					var part struct {
						Text string `json:"text"`
					}
					if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &part); err != nil {
						t.Fatal(err)
					}
					text += part.Text
					chunks++
					if chunks == 1 {
						// The fake agent cannot finish until the HTTP client acknowledges
						// this first delta. This also verifies Flush, not just callbacks.
						var count int
						if err := s.Store.DB.QueryRow("SELECT count(*) FROM messages WHERE document_id='doc'").Scan(&count); err != nil || count != 1 {
							t.Fatalf("premature persistence: %d %v", count, err)
						}
						if err := os.WriteFile(ack, []byte("received"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				if scanner.Err() != nil || !done || chunks != 2 || text != "你好，世界" {
					t.Fatalf("SSE: done=%t chunks=%d text=%q error=%v", done, chunks, text, scanner.Err())
				}
				stored := request(t, s, "GET", "/api/documents/doc/messages", nil)
				var messages []Message
				if json.Unmarshal(stored.Body.Bytes(), &messages) != nil || len(messages) != 2 || messages[1].Content != text {
					t.Fatalf("stored answer: %s", stored.Body.String())
				}
			})
		}
	}
}

func TestChatPartialFailureIsNotSaved(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "kimi"} {
		t.Run(provider, func(t *testing.T) {
			fakeAgent(t, provider, "fail-after")
			s, _ := imageFixture(t)
			config := AIConfig{Primary: provider, Models: map[string]string{}, Capabilities: map[string]Capability{}}
			config.Capabilities[provider] = Capability{Text: true, Vision: true, Fingerprint: configPrint(config, provider)}
			if err := s.writeAIConfig(config); err != nil {
				t.Fatal(err)
			}
			res := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(fmt.Sprintf(`{"provider":%q,"prompt":"test","context":"","attachments":["p1-b1"]}`, provider)))
			if !strings.Contains(res.Body.String(), "event: delta") || !strings.Contains(res.Body.String(), "event: error") || strings.Contains(res.Body.String(), "event: done") {
				t.Fatal(res.Body.String())
			}
			var count int
			if err := s.Store.DB.QueryRow("SELECT count(*) FROM messages WHERE document_id='doc'").Scan(&count); err != nil || count != 1 {
				t.Fatalf("partial assistant answer persisted: %d %v", count, err)
			}
		})
	}
}

// Opt in with a provider name; uses only a synthetic prompt and the CLI login.
func TestLiveAgentStreaming(t *testing.T) {
	provider := os.Getenv("NEOREAD_TEST_STREAM_PROVIDER")
	if !validAgent(provider) {
		t.Skip("set NEOREAD_TEST_STREAM_PROVIDER to codex, claude, or kimi")
	}
	s := testServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	start := time.Now()
	var first, last time.Duration
	var parts []string
	answer, err := invokeCLI(ctx, s.Store.Root, provider, "", AIInput{Prompt: "Explain how HTTP streaming works in approximately 150 words. Do not use any tools."}, func(part string) {
		last = time.Since(start)
		if len(parts) == 0 {
			first = last
		}
		parts = append(parts, part)
	})
	if err != nil || len(parts) < 2 || strings.Join(parts, "") != answer {
		t.Fatalf("chunks=%d error=%v", len(parts), err)
	}
	t.Logf("provider=%s chunks=%d bytes=%d first=%s last=%s completed=%s", provider, len(parts), len(answer), first, last, time.Since(start))
}

// Uses only original prompts and generated pixels, never library content.
func TestLiveGenerationAdapter(t *testing.T) {
	provider := os.Getenv("NEOREAD_TEST_STREAM_PROVIDER")
	if !validAgent(provider) {
		t.Skip("set NEOREAD_TEST_STREAM_PROVIDER")
	}
	for _, vision := range []bool{false, true} {
		t.Run(fmt.Sprintf("image=%t", vision), func(t *testing.T) {
			s := testServer(t)
			in := AIInput{Prompt: "Explain HTTP streaming in roughly 100 words. Do not use tools."}
			if vision {
				im, _, err := visionProbe()
				if err != nil {
					t.Fatal(err)
				}
				in = AIInput{Prompt: "Describe the colors and layout of the attached synthetic image in roughly 100 words. Do not use tools.", Image: im}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			start := time.Now()
			var first time.Duration
			count := 0
			var text string
			result, err := runAttempt(ctx, cliAdapter{s.Store.Root, provider, ""}, in, func(e ProviderEvent) error {
				if count == 0 {
					first = time.Since(start)
				}
				count++
				text += e.Text
				return nil
			})
			if err != nil || result.Text != text || count < 2 {
				t.Fatalf("chunks=%d err=%v", count, err)
			}
			t.Logf("provider=%s image=%t chunks=%d bytes=%d first=%s complete=%s", provider, vision, count, len(text), first, time.Since(start))
		})
	}
	t.Run("cancel", func(t *testing.T) {
		s := testServer(t)
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		count := 0
		var canceled time.Time
		_, err := runAttempt(ctx, cliAdapter{s.Store.Root, provider, ""}, AIInput{Prompt: "Write a detailed 1000 word explanation of HTTP streaming. Do not use tools."}, func(e ProviderEvent) error { count++; canceled = time.Now(); cancel(); return nil })
		if !errors.Is(err, context.Canceled) || count != 1 {
			t.Fatalf("chunks=%d err=%v", count, err)
		}
		t.Logf("provider=%s cancel_return=%s", provider, time.Since(canceled))
	})
}

// These fixtures also serve the frontend replay harness. Round-trip each frame
// through the real writer to guard event names, JSON shapes and append semantics.
func TestFrozenStreamFixtures(t *testing.T) {
	raw, err := os.ReadFile("../../../../docs/fixtures/agent-streaming-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name, Wire, Text string
		Terminal         *string
	}
	if err = json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			recorder := httptest.NewRecorder()
			writer := &chatStreamWriter{w: recorder, cancel: cancel}
			scan := bufio.NewScanner(strings.NewReader(fixture.Wire))
			event, text, terminal := "", "", ""
			for scan.Scan() {
				line := scan.Text()
				if strings.HasPrefix(line, "event: ") {
					event = strings.TrimPrefix(line, "event: ")
				}
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var value map[string]any
				if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &value); err != nil {
					t.Fatal(err)
				}
				if event != "activity" && event != "draft" && len(value) != 1 {
					t.Fatal("unexpected fields")
				}
				switch event {
				case "reasoning":
					if _, ok := value["text"].(string); !ok {
						t.Fatal(value)
					}
				case "draft":
					if len(value) != 2 {
						t.Fatal(value)
					}
					if _, ok := value["text"].(string); !ok {
						t.Fatal(value)
					}
					if _, ok := value["reset"].(bool); !ok {
						t.Fatal(value)
					}
				case "activity":
					if len(value) < 2 || len(value) > 3 || (value["phase"] != "indexing" && value["phase"] != "reading" && value["phase"] != "writing") {
						t.Fatal(value)
					}
					if _, ok := value["message"].(string); !ok {
						t.Fatal(value)
					}
					if len(value) == 3 {
						if _, ok := value["tool"].(string); !ok {
							t.Fatal(value)
						}
					}
				case "status":
					if value["status"] != "reading" && value["status"] != "reading-image" {
						t.Fatal(value)
					}
				case "delta":
					part, ok := value["text"].(string)
					if !ok {
						t.Fatal(value)
					}
					text += part
				case "fallback":
					if _, ok := value["message"].(string); !ok {
						t.Fatal(value)
					}
				case "error":
					terminal = event
					if _, ok := value["error"].(string); !ok {
						t.Fatal(value)
					}
				case "done":
					terminal = event
					if value["ok"] != true {
						t.Fatal(value)
					}
				default:
					t.Fatal(event)
				}
				if err = writer.send(event, value); err != nil {
					t.Fatal(err)
				}
			}
			if scan.Err() != nil || text != fixture.Text || recorder.Body.String() != fixture.Wire || ctx.Err() != nil {
				t.Fatal("fixture does not match wire")
			}
			if fixture.Terminal != nil {
				if terminal != *fixture.Terminal || writer.send("delta", map[string]string{"text": "late"}) == nil {
					t.Fatal("terminal mismatch")
				}
			} else if terminal != "" {
				t.Fatal("unexpected terminal")
			}
		})
	}
}

func TestClaudeEventEnvelope(t *testing.T) {
	for _, frame := range []string{
		`{"type":"system","subtype":"ui_invalidate","event":"settings_changed"}`,
		`{"type":"system","subtype":"init"}`,
	} {
		text, done, failed := claudeEvent([]byte(frame))
		if text != "" || done || failed {
			t.Fatalf("informational envelope rejected: %s", frame)
		}
	}
	for _, frame := range []string{
		`{"type":"stream_event","event":"invalid"}`,
		`{"type":"stream_event","event":{"delta":{"text":42}}}`,
		`{"type":"result","subtype":"success","is_error":true}`,
		`{"type":"result","subtype":"error_during_execution"}`,
		`not json`,
	} {
		_, _, failed := claudeEvent([]byte(frame))
		if !failed {
			t.Fatalf("invalid/error frame accepted: %s", frame)
		}
	}
}

func TestClaudeUserSettingsWithoutLogin(t *testing.T) {
	fakeAgent(t, "claude", "user-settings")
	if providerStatus(context.Background(), "claude").Authenticated {
		t.Fatal("fixture must report no subscription login")
	}
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	answer, err := invokeCLI(t.Context(), store.Root, "claude", "", AIInput{Prompt: "hello"}, nil)
	if err != nil || answer != "你好，世界" {
		t.Fatalf("user-configured CLI should work without subscription login: %q %v", answer, err)
	}
}
