package neoread

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestFormulaMarkdownRequiresFormulaOnlyResponse(t *testing.T) {
	for _, valid := range []string{"$$\nx=1\n$$", "$$x=1$$\n\n$$y=2$$"} {
		if _, err := formulaMarkdown(valid); err != nil {
			t.Fatal(err)
		}
	}
	for _, invalid := range []string{"只有解释", "$$ $$", "$$x$$\n$$ $$", "解释：$$x$$", "$$x$$\n解释", "```latex\n$$x$$\n```"} {
		if _, err := formulaMarkdown(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}
func TestImageAssetsTranslateOnlyCaption(t *testing.T) {
	for _, label := range []string{"table", "image", "chart"} {
		b := PDFBlock{Label: label, Image: "assets/p1-b1.png", Text: "INTERNAL CELLS", Caption: "Table 5. Results."}
		if translationSource(b) != b.Caption {
			t.Fatal("caption missing")
		}
		b.Caption = ""
		if translationSource(b) != "" {
			t.Fatal("asset body leaked")
		}
	}
}
func formulaFixture(t *testing.T) (*Server, Processing, layoutManifest) {
	t.Helper()
	s, p := processingFixture(t)
	s.Token = "test-secret"
	m, _ := s.readLayout("doc")
	formula := m.Blocks[0]
	formula.Label = "display_formula"
	formula.Text = "FORMULA OCR"
	table := formula
	table.ID = "p1-b2"
	table.Label = "table"
	table.Image = "assets/p1-b2.png"
	table.Text = "INTERNAL CELLS"
	table.Caption = "Table 5. Results."
	m.Blocks = []PDFBlock{formula, table}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(s.analysisDir("doc"), "manifest.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, b := range m.Blocks {
		if err := os.WriteFile(filepath.Join(s.analysisDir("doc"), b.Image), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return s, p, m
}
func configureFormulaTest(t *testing.T, s *Server, textURL, imageURL string) {
	t.Helper()
	configureTranslationTest(t, s, textURL)
	config := s.aiConfig()
	config.ImageAPI = APIConnection{URL: imageURL, Model: "test"}
	config.Capabilities["image-api"] = Capability{Text: true, Vision: true, Fingerprint: configPrint(config, "image-api")}
	if err := s.writeAIConfig(config); err != nil {
		t.Fatal(err)
	}
}
func waitFormulaSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for concurrent provider")
	}
}
func TestFormulaConversionDoesNotBlockCaptionTranslation(t *testing.T) {
	s, p, m := formulaFixture(t)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	formulaStarted := make(chan struct{})
	textStarted := make(chan struct{})
	var imageCalls atomic.Int32
	images := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imageCalls.Add(1)
		var body struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		isMath := strings.Contains(string(body.Messages[len(body.Messages)-1].Content), "FORMULA OCR")
		w.Header().Set("Content-Type", "text/event-stream")
		if isMath {
			close(formulaStarted)
			sendTranslationDelta(w, "$$\nx_t=yx\n$$")
			w.(http.Flusher).Flush()
			<-release
		} else {
			t.Error("table attachment sent to the vision provider")
		}
		finishTranslationStream(w)
	}))
	defer images.Close()
	defer unblock()
	text := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(textStarted)
		input := readTranslationInput(t, r)
		if len(input.Batch.Paragraphs) != 1 || input.Batch.Paragraphs[0].Source != m.Blocks[1].Caption {
			t.Error("asset body sent to text translation")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, para := range input.Batch.Paragraphs {
			sendTranslationDelta(w, translationParagraphLine(para))
		}
		finishTranslationStream(w)
	}))
	defer text.Close()
	configureFormulaTest(t, s, text.URL, images.URL)
	if err := s.learnPDF(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if p.TranslationsTotal != 2 {
		t.Fatalf("wrong translation total: %+v", p)
	}
	done := make(chan error, 1)
	go func() { done <- s.processTranslation(context.Background(), &p) }()
	waitFormulaSignal(t, formulaStarted)
	waitFormulaSignal(t, textStarted)
	deadline := time.Now().Add(10 * time.Second)
	for {
		items, err := s.translations("doc", m)
		if err != nil {
			t.Fatal(err)
		}
		if items[1].Status == "complete" {
			if items[0].Status != "running" || items[0].FormulaMarkdown != "" {
				t.Fatal("promoted partial formula before provider completion")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("caption blocked by slow formula")
		}
		time.Sleep(5 * time.Millisecond)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if p.Status != "complete" || p.TranslationsDone != 2 {
		t.Fatalf("%+v", p)
	}
	items, _ := s.translations("doc", m)
	if items[0].FormulaMarkdown != "$$\nx_t=yx\n$$\n" {
		t.Fatalf("%+v", items[0])
	}
	for _, relative := range []string{"formulas", "transcripts/p1-b1.md"} {
		if _, err := os.Stat(filepath.Join(s.analysisDir("doc"), relative)); !os.IsNotExist(err) {
			t.Fatalf("obsolete formula artifact: %s", relative)
		}
	}
	if err := s.processTranslation(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if imageCalls.Load() != 1 {
		t.Fatal("repeated completed formula conversion")
	}
}
func TestFormulaFailurePreservesCaptionAndCanRetry(t *testing.T) {
	s, p, m := formulaFixture(t)
	var valid atomic.Bool
	images := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		response := "只有解释，没有公式"
		if valid.Load() {
			response = "$$\nx=1\n$$"
		}
		sendTranslationDelta(w, response)
		finishTranslationStream(w)
	}))
	defer images.Close()
	text := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		input := readTranslationInput(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, para := range input.Batch.Paragraphs {
			sendTranslationDelta(w, translationParagraphLine(para))
		}
		finishTranslationStream(w)
	}))
	defer text.Close()
	configureFormulaTest(t, s, text.URL, images.URL)
	if err := s.learnPDF(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if err := s.processTranslation(context.Background(), &p); err == nil {
		t.Fatal("accepted invalid math")
	}
	items, _ := s.translations("doc", m)
	if items[0].Status != "failed" || items[0].FormulaMarkdown != "" || items[1].Status != "complete" {
		t.Fatalf("%+v", items)
	}
	valid.Store(true)
	w := request(t, s, "POST", "/api/documents/doc/processing", nil)
	if w.Code != 200 {
		t.Fatalf("retry failed: %s", w.Body.String())
	}
	p, _ = s.Store.processing("doc")
	if err := s.processTranslation(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	items, _ = s.translations("doc", m)
	if items[0].Status != "complete" {
		t.Fatalf("%+v", items)
	}
}

func TestAssetBodyExcludedFromTranslationFrontMatter(t *testing.T) {
	m := layoutManifest{Blocks: []PDFBlock{{ID: "p1-b1", Page: 1, Label: "table", Text: "PRIVATE TABLE CELLS", Caption: "Table caption."}, {ID: "p1-b2", Page: 1, Label: "display_formula", Text: "FORMULA OCR"}}}
	prompt := translationPrompt(Document{Title: "Test"}, m, translationBatch{}, "")
	if strings.Contains(prompt, "PRIVATE TABLE CELLS") || strings.Contains(prompt, "FORMULA OCR") {
		t.Fatal("asset body leaked through metadata context")
	}
}

func TestFormulaCancellationResumesWithoutSavingPartialResult(t *testing.T) {
	s, _, m := formulaFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	images := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sendTranslationDelta(w, "$$x=1$$")
		w.(http.Flusher).Flush()
		close(started)
		<-release
		finishTranslationStream(w)
	}))
	defer images.Close()
	defer close(release)
	configureFormulaTest(t, s, images.URL, images.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.convertFormulas(ctx, "doc", m, nil) }()
	waitFormulaSignal(t, started)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancellation was ignored")
	}
	items, _ := s.translations("doc", m)
	if items[0].Status != "pending" || items[0].FormulaMarkdown != "" {
		t.Fatalf("partial formula saved: %+v", items[0])
	}
}

func TestFormulaRetranslationRetainsSavedAgentAfterCurrentSelectionIsCleared(t *testing.T) {
	s, p, m := formulaFixture(t)
	completedFormulaCaption(t, s, m)
	var calls atomic.Int32
	images := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		sendTranslationDelta(w, "$$x=1$$")
		finishTranslationStream(w)
	}))
	defer images.Close()
	config := AIConfig{ImageAPI: APIConnection{URL: images.URL, Model: "test"}, Capabilities: map[string]Capability{}}
	config.Capabilities["image-api"] = Capability{Text: true, Vision: true, Fingerprint: configPrint(config, "image-api")}
	if err := s.writeAIConfig(config); err != nil {
		t.Fatal(err)
	}
	options := translationRunOptions{Provider: "codex", Model: "saved-translation-model", Effort: "high"}
	if _, err := s.Store.DB.Exec("INSERT INTO translation_restarts(document_id,options) VALUES(?,?)", "doc", jsonText(options)); err != nil {
		t.Fatal(err)
	}
	p.Enabled, p.Retranslating = true, true
	p.lane = "translating"
	p.Translating = &ProcessingStage{Status: "queued"}
	if err := s.Store.saveProcessing(p); err != nil {
		t.Fatal(err)
	}
	if err := s.translateContent(t.Context(), &p, m); err != nil {
		t.Fatal(err)
	}
	items, err := s.translations("doc", m)
	if err != nil || items[0].Status != "complete" || items[0].FormulaMarkdown != "$$x=1$$\n" || items[1].Sentences[0].Target != "已保存图注" {
		t.Fatal("saved formula/prose missing", items, err)
	}
	if calls.Load() != 1 {
		t.Fatal("formula conversion repeated", calls.Load())
	}
	if current := s.aiConfig(); current.Primary != "" {
		t.Fatal("task changed global Agent selection")
	}
}

func TestFormulaTimeoutRetryIsBoundedAndCancelable(t *testing.T) {
	for _, mode := range []string{"recover", "exhaust", "upstream", "invalid-math", "cancel-backoff", "parent-deadline", "already-canceled"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if mode == "already-canceled" {
					cancel()
				}
				if mode == "parent-deadline" {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, time.Minute)
					defer stop()
				}
				var cancelTimer *time.Timer
				defer func() {
					if cancelTimer != nil {
						cancelTimer.Stop()
					}
				}()
				calls := 0
				service := &GenerationService{
					primary: "codex",
					connections: map[string]generationConnection{
						"codex": {
							VerifiedText: true, VerifiedVision: true,
							Adapter: adapterFunc(func(call context.Context, _ GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
								calls++
								switch mode {
								case "upstream":
									return GenerateResult{}, generationError(ErrorUpstream, "temporary upstream error")
								case "invalid-math":
									_ = emit(ProviderEvent{Text: "not a formula"})
									return GenerateResult{Text: "not a formula", FinishReason: "stop"}, nil
								case "cancel-backoff":
									cancelTimer = time.AfterFunc(500*time.Millisecond, cancel)
									return GenerateResult{}, context.DeadlineExceeded
								}
								if calls == 1 || mode == "exhaust" {
									_ = emit(ProviderEvent{Text: "$$unfinished"})
									<-call.Done()
									return GenerateResult{}, call.Err()
								}
								_ = emit(ProviderEvent{Text: "$$x=1$$"})
								return GenerateResult{Text: "$$x=1$$", FinishReason: "stop"}, nil
							}),
						},
					},
				}
				start := time.Now()
				text, err := generateFormula(ctx, service, AIInput{Image: []byte{1}})
				switch mode {
				case "recover":
					if err != nil || calls != 2 || text != "$$x=1$$\n" {
						t.Fatalf("calls=%d text=%q err=%v", calls, text, err)
					}
				case "exhaust":
					if errorKind(err) != ErrorTimeout || calls != 2 || text != "" || !strings.Contains(err.Error(), "重试") {
						t.Fatalf("calls=%d text=%q err=%v", calls, text, err)
					}
					if elapsed := time.Since(start); elapsed != 6*time.Minute+time.Second {
						t.Fatalf("unexpected retry budget: %v", elapsed)
					}
				case "cancel-backoff", "already-canceled":
					wantCalls := 1
					if mode == "already-canceled" {
						wantCalls = 0
					}
					if !errors.Is(err, context.Canceled) || calls != wantCalls || text != "" {
						t.Fatalf("calls=%d text=%q err=%v", calls, text, err)
					}
				default:
					if err == nil || calls != 1 || text != "" {
						t.Fatalf("non-timeout retried: calls=%d text=%q err=%v", calls, text, err)
					}
				}
			})
		})
	}
}

func TestFormulaTimeoutRetrySavesOnlyCompletedResult(t *testing.T) {
	s, _, manifest := formulaFixture(t)
	var calls atomic.Int32
	images := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			return
		}
		items, err := s.translations("doc", manifest)
		if err != nil || items[0].Status != "running" || items[0].FormulaMarkdown != "" {
			t.Errorf("retry exposed a failed or partial result: %+v %v", items, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		sendTranslationDelta(w, "$$x=1$$")
		finishTranslationStream(w)
	}))
	defer images.Close()
	configureFormulaTest(t, s, images.URL, images.URL)
	if err := s.convertFormulas(t.Context(), "doc", manifest, nil); err != nil {
		t.Fatal(err)
	}
	items, err := s.translations("doc", manifest)
	if err != nil || calls.Load() != 2 || items[0].Status != "complete" || items[0].FormulaMarkdown != "$$x=1$$\n" || items[1].Status != "pending" {
		t.Fatalf("calls=%d items=%+v err=%v", calls.Load(), items, err)
	}
	if !capable(s.aiConfig(), "image-api", true) {
		t.Fatal("formula timeout revoked verified image capability")
	}
}

func completedFormulaCaption(t *testing.T, s *Server, m layoutManifest) {
	t.Helper()
	item := newTranslation(m.Blocks[1])
	item.Status = "complete"
	item.Sentences = []TranslationSentence{{Source: m.Blocks[1].Caption, Target: "已保存图注"}}
	if err := s.saveTranslation("doc", item); err != nil {
		t.Fatal(err)
	}
}

func TestFormulaCapabilityWaitResumesWithoutFailingSavedProse(t *testing.T) {
	s, p, m := formulaFixture(t)
	completedFormulaCaption(t, s, m)
	if err := s.writeAIConfig(AIConfig{Primary: "codex", Capabilities: map[string]Capability{}}); err != nil {
		t.Fatal(err)
	}
	p.Enabled = true
	p.Translating = &ProcessingStage{Status: "queued"}
	if err := s.Store.saveProcessing(p); err != nil {
		t.Fatal(err)
	}
	if err := s.processTranslation(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != "waiting" || p.Translating.Status != "waiting" || !strings.Contains(p.Detail, "图像能力") {
		t.Fatalf("formula wait: %+v", p)
	}
	items, err := s.translations("doc", m)
	if err != nil || items[0].Status != "pending" || items[0].Error != "" || items[1].Status != "complete" {
		t.Fatalf("lost waiting formula or saved caption: %+v %v", items, err)
	}
	images := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sendTranslationDelta(w, "$$x=1$$")
		finishTranslationStream(w)
	}))
	defer images.Close()
	configureFormulaTest(t, s, images.URL, images.URL)
	s.wakeProcessing()
	p, err = s.Store.processing("doc")
	if err != nil || p.Status != "queued" {
		t.Fatal(p, err)
	}
	if err = s.processTranslation(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	items, err = s.translations("doc", m)
	if err != nil || p.Status != "complete" || items[0].FormulaMarkdown != "$$x=1$$\n" || items[1].Sentences[0].Target != "已保存图注" {
		t.Fatalf("failed to resume formula only: %+v %+v %v", p, items, err)
	}
}

func TestReopenedReparseRecoversOnlyLegacyFormulaCapabilityFailures(t *testing.T) {
	for _, mode := range []string{"waiting", "ready", "paused", "manual", "upstream"} {
		t.Run(mode, func(t *testing.T) {
			s, p, m := formulaFixture(t)
			completedFormulaCaption(t, s, m)
			m.RetranslationRun = strings.Repeat("a", 32)
			raw, _ := json.Marshal(m)
			if err := os.WriteFile(filepath.Join(s.analysisDir("doc"), "manifest.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			item := newTranslation(m.Blocks[0])
			item.Status = "failed"
			item.Error = "主 Agent 尚未通过所需能力测试"
			if mode == "upstream" {
				item.Error = "provider failed"
			}
			if err := s.saveTranslation("doc", item); err != nil {
				t.Fatal(err)
			}
			p.Enabled = mode != "paused"
			p.ManualTranslation = mode == "manual"
			p.LayoutRevision = m.RetranslationRun
			p.Translating = &ProcessingStage{Status: "failed", Detail: item.Error}
			aggregateProcessing(&p)
			if err := s.Store.saveProcessing(p); err != nil {
				t.Fatal(err)
			}
			if mode == "ready" {
				c := AIConfig{Primary: "codex", ImageAPI: APIConnection{URL: "http://127.0.0.1:1", Model: "test"}, Capabilities: map[string]Capability{}}
				c.Capabilities["image-api"] = Capability{Text: true, Vision: true, Fingerprint: configPrint(c, "image-api")}
				if err := s.writeAIConfig(c); err != nil {
					t.Fatal(err)
				}
			}
			d, err := s.Store.Document("doc")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.reparsedPDFLayout(d); err != nil {
				t.Fatal(err)
			}
			saved, err := s.Store.processing("doc")
			if err != nil {
				t.Fatal(err)
			}
			items, err := s.translations("doc", m)
			if err != nil {
				t.Fatal(err)
			}
			want := "failed"
			if mode == "waiting" {
				want = "waiting"
			}
			if mode == "ready" {
				want = "queued"
			}
			if saved.Translating.Status != want {
				t.Fatalf("stage %s want %s", saved.Translating.Status, want)
			}
			status := "failed"
			if mode == "waiting" || mode == "ready" {
				status = "pending"
			}
			if items[0].Status != status || items[1].Status != "complete" {
				t.Fatalf("changed unrelated data: %+v", items)
			}
		})
	}
}
