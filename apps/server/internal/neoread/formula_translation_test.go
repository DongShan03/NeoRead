package neoread

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"neoread.local/server/internal/diagnostics"
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

func TestInlineFormulaAlwaysKeepsSingleDollarDelimiters(t *testing.T) {
	for _, value := range []string{`\tau_i`, `$\tau_i$`, "$$\n\\tau_i\n$$"} {
		got, err := inlineFormulaMarkdown(value)
		if err != nil || got != `$\tau_i$` {
			t.Fatal(got, err)
		}
	}
	for _, value := range []string{"", "$$ $$", "$x$ and $y$", "```latex\nx\n```"} {
		if _, err := inlineFormulaMarkdown(value); err == nil {
			t.Fatal("accepted incomplete formula", value)
		}
	}
}

func fakeTranslationMathCrop(t *testing.T) {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 20, 20))); err != nil {
		t.Fatal(err)
	}
	pngPath := filepath.Join(t.TempDir(), "crop.png")
	if err := os.WriteFile(pngPath, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "render-pages.mjs")
	body := `while [ $# -gt 0 ]; do case "$1" in --output) out="$2"; shift;; --pages) page="$2"; shift;; esac; shift; done
cp "$NEOREAD_TEST_MATH_CROP" "$out/page-$page.png"
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEOREAD_TEST_MATH_CROP", pngPath)
	t.Setenv("NEOREAD_NODE", "/bin/sh")
	t.Setenv("NEOREAD_PROCESSOR", filepath.Join(filepath.Dir(script), "main.mjs"))
}

func TestTranslationMathRecoveryPreservesVerifiedFormulasAndOriginalAnchors(t *testing.T) {
	for _, mode := range []string{"complete", "missing_token", "extra_formula", "invalid_math", "invalid_source_hash", "truncated", "changed_source", "invalid_ocr", "unverified_image", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			s, _, m := translationFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			m.Blocks = m.Blocks[:1]
			m.Blocks[0].Text = `The activation is $\hat{a}_n$. The threshold is 𝜏.`
			if err := atomicFile(filepath.Join(s.analysisDir("doc"), "manifest.json"), []byte(jsonText(m))); err != nil {
				t.Fatal(err)
			}
			fakeTranslationMathCrop(t)
			var texts, images atomic.Int32
			imageAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				images.Add(1)
				var input map[string]any
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				if !strings.Contains(jsonText(input), "image_url") {
					t.Error("no crop sent to recognition")
				}
				if mode == "changed_source" {
					if err := os.WriteFile(s.Store.File(Document{ID: "doc", Type: "pdf", Library: "papers"}), []byte("changed PDF"), 0600); err != nil {
						t.Error(err)
					}
				}
				if mode == "canceled" {
					cancel()
				}
				if mode == "invalid_ocr" {
					writeAPIReply(w, `{"formulas":[{"index":0,"latex":"x"},{"index":0,"latex":"y"}]}`)
					return
				}
				// Deliberately return display delimiters: recovery must insert inline math.
				writeAPIReply(w, jsonText(map[string]any{"formulas": []any{map[string]any{"index": 0, "latex": `$$\hat{a}_n$$`}, map[string]any{"index": 1, "latex": `\tau`}}}))
			}))
			defer imageAPI.Close()
			textAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				texts.Add(1)
				batch := readTranslationInput(t, r).Batch
				if len(batch.Paragraphs) != 1 {
					t.Error("unexpected paragraphs")
					return
				}
				para := batch.Paragraphs[0]
				pairs := []TranslationSentence{}
				for _, part := range translationSentenceParts(para.Source) {
					target := `错误：$a_n$ 和 $t$。`
					if strings.Contains(part, "NEOREAD_MATH_") {
						token := regexp.MustCompile(`NEOREAD_MATH_[a-f0-9]+_[0-9]+`).FindString(part)
						target = "译文 " + token + "。"
						if mode == "missing_token" {
							target = "公式已丢失。"
						}
						if mode == "extra_formula" {
							target += `$z$`
						}
						if mode == "invalid_math" {
							target += `$\Gamma$`
						}
					}
					pairs = append(pairs, TranslationSentence{Source: strings.TrimSpace(part), Target: target})
				}
				hash := para.SourceHash
				if mode == "invalid_source_hash" {
					hash = "wrong-version"
				}
				line := jsonText(map[string]any{"blockId": para.BlockID, "sourceHash": hash, "sentences": pairs}) + "\n"
				if mode == "truncated" && strings.Contains(para.Source, "NEOREAD_MATH_") {
					w.Header().Set("Content-Type", "text/event-stream")
					sendTranslationDelta(w, line)
					return
				}
				writeAPIReply(w, line)
			}))
			defer textAPI.Close()
			configureFormulaTest(t, s, textAPI.URL, imageAPI.URL)
			if mode == "unverified_image" {
				config := s.aiConfig()
				delete(config.Capabilities, "image-api")
				if err := s.writeAIConfig(config); err != nil {
					t.Fatal(err)
				}
			}
			doc, err := s.Store.Document("doc")
			if err != nil {
				t.Fatal(err)
			}
			service := s.taskGenerationService(s.aiConfig(), taskTranslation)
			paragraph := m.translationParagraphs()[0]
			if mode == "invalid_math" || mode == "invalid_source_hash" {
				logs, e := diagnostics.New(t.TempDir(), "math-recovery-test")
				if e != nil {
					t.Fatal(e)
				}
				s.SetDiagnostics(logs)
				_, err := s.recoverTranslationMath(ctx, doc, m, paragraph, service)
				reason := errTranslationMath.Error()
				if mode == "invalid_source_hash" {
					reason = "译文原文版本不匹配"
				}
				if err == nil || errorKind(err) != ErrorProtocol || err.Error() != "公式修复后的译文未完整逐句对应："+reason {
					t.Fatal("lost decoder failure reason or protocol classification", err)
				}
				files, e := filepath.Glob(filepath.Join(logs.Dir, "*.jsonl"))
				if e != nil || len(files) != 1 {
					t.Fatal(files, e)
				}
				data, e := os.ReadFile(files[0])
				if e != nil || !bytes.Contains(data, []byte(`"event":"translation.math_recovery.finished"`)) || !bytes.Contains(data, []byte(`"errorCode":"protocol"`)) || !bytes.Contains(data, []byte(`"phase":"translating"`)) || bytes.Contains(data, []byte(reason)) {
					t.Fatal("recovery lost diagnostic classification or logged raw failure", string(data), e)
				}
				if texts.Load() != 1 || images.Load() != 1 {
					t.Fatal("unexpected recovery calls", texts.Load(), images.Load())
				}
				return
			}
			err = s.translateBatch(ctx, doc, m, translationBatch{Paragraphs: []translationParagraph{paragraph}}, service, func() {})
			if mode == "canceled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			items, err := s.translations("doc", m)
			if err != nil || len(items) != 1 {
				t.Fatal(items, err)
			}
			wantTexts, wantImages := int32(3), int32(1)
			if mode == "invalid_ocr" || mode == "canceled" || mode == "unverified_image" {
				wantTexts = 2
			}
			if mode == "unverified_image" {
				wantImages = 0
			}
			if texts.Load() != wantTexts || images.Load() != wantImages {
				t.Fatal("unexpected retry count", texts.Load(), images.Load())
			}
			if mode == "canceled" {
				if items[0].Status != "pending" {
					t.Fatal("canceled recovery did not stay retryable", items[0])
				}
				return
			}
			if mode != "complete" {
				if items[0].Status != "failed" {
					t.Fatal("promoted invalid recovery", items[0])
				}
				return
			}
			item := items[0]
			if item.Status != "complete" || item.SourceHash != paragraph.SourceHash || len(item.Sentences) != 2 {
				t.Fatal(item)
			}
			if !strings.Contains(item.Sentences[0].Target, `$\hat{a}_n$`) || !strings.Contains(item.Sentences[1].Target, `$\tau$`) || strings.Contains(jsonText(item.Sentences), "$$") {
				t.Fatal("lost formula or displayed it as a block", item)
			}
			if item.Sentences[1].Anchor != `The threshold is 𝜏.` {
				t.Fatal("lost native PDF anchor", item)
			}
			if repaired := repairCachedTranslationAnchors(item, paragraph.Source); jsonText(repaired) != jsonText(item) {
				t.Fatal("reading cache changed recovered sentences", repaired)
			}
		})
	}
}

func TestFormulaRecognitionSpansPreserveLegacyMathScriptsAndProse(t *testing.T) {
	text := `Let 𝜏_i and ℝ represent the threshold and domain, with $\hat{x}$ unchanged.`
	spans := inlineMathSpans(text)
	if len(spans) != 3 || text[spans[0].Start:spans[0].End] != `𝜏_i` || text[spans[1].Start:spans[1].End] != "ℝ" || spans[2].Formula != `\hat{x}` {
		t.Fatal(spans)
	}
}

func TestTranslationMathRecoveryRestoresDroppedCompatibilityGlyphs(t *testing.T) {
	for _, tt := range []struct {
		name, source, glyph string
		formulas            []string
	}{
		{"iid_increment", deltaIIDParagraph, "\u2206", []string{`\Delta`}},
		{"control_increment", deltaControlParagraph, "\u2206", []string{`\Delta`, `P_{n}^{q}`}},
		{"micro", "The distance is 5 \u00b5m.", "\u00b5", []string{`\mu`}},
		{"ohm", "The resistance is 5 \u2126.", "\u2126", []string{`\Omega`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, _, m := translationFixture(t)
			m.Blocks = m.Blocks[:1]
			m.Blocks[0].Text = tt.source
			if err := atomicFile(filepath.Join(s.analysisDir("doc"), "manifest.json"), []byte(jsonText(m))); err != nil {
				t.Fatal(err)
			}
			fakeTranslationMathCrop(t)
			var texts, images atomic.Int32
			imageAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				images.Add(1)
				var request struct {
					Messages []struct {
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				var input struct {
					Formulas []recognizedInlineFormula `json:"formulas"`
				}
				var content []struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(request.Messages[len(request.Messages)-1].Content, &content); err != nil {
					t.Error(err)
					return
				}
				_, raw, ok := strings.Cut(content[0].Text, "输入资料：\n")
				if !ok || json.Unmarshal([]byte(raw), &input) != nil || len(input.Formulas) != len(tt.formulas) {
					t.Error("recovery did not recognize all math spans", input)
					return
				}
				output := []map[string]any{}
				for i, formula := range tt.formulas {
					if strings.TrimSpace(input.Formulas[i].Formula) != formula {
						t.Error("unexpected formula", input.Formulas[i])
					}
					output = append(output, map[string]any{"index": i, "latex": formula})
				}
				writeAPIReply(w, jsonText(map[string]any{"formulas": output}))
			}))
			defer imageAPI.Close()
			textAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				texts.Add(1)
				para := readTranslationInput(t, r).Batch.Paragraphs[0]
				pairs := []TranslationSentence{}
				for _, part := range translationSentenceParts(para.Source) {
					part = strings.TrimSpace(part)
					// Both ordinary attempts drop the glyph. Recovery must protect
					// it along with any existing delimited formulas.
					target := strings.ReplaceAll(part, tt.glyph, "")
					if strings.Contains(para.Source, "NEOREAD_MATH_") {
						tokens := regexp.MustCompile(`NEOREAD_MATH_[a-f0-9]+_[0-9]+`).FindAllString(part, -1)
						target = "译文 " + strings.Join(tokens, " ") + "。"
					}
					pairs = append(pairs, TranslationSentence{Source: part, Target: target})
				}
				writeAPIReply(w, jsonText(map[string]any{"blockId": para.BlockID, "sourceHash": para.SourceHash, "sentences": pairs})+"\n")
			}))
			defer textAPI.Close()
			configureFormulaTest(t, s, textAPI.URL, imageAPI.URL)
			doc, err := s.Store.Document("doc")
			if err != nil {
				t.Fatal(err)
			}
			paragraph := m.translationParagraphs()[0]
			service := s.taskGenerationService(s.aiConfig(), taskTranslation)
			if err := s.translateBatch(t.Context(), doc, m, translationBatch{Paragraphs: []translationParagraph{paragraph}}, service, func() {}); err != nil {
				t.Fatal(err)
			}
			items, err := s.translations("doc", m)
			if err != nil || len(items) != 1 || items[0].Status != "complete" {
				t.Fatal("dropped glyph left paragraph failed", items, err)
			}
			if texts.Load() != 3 || images.Load() != 1 {
				t.Fatal("unexpected recovery calls", texts.Load(), images.Load())
			}
			parts := translationSentenceParts(tt.source)
			if len(items[0].Sentences) != len(parts) {
				t.Fatal("lost sentence", items[0])
			}
			var targets strings.Builder
			for i, pair := range items[0].Sentences {
				anchor := pair.Anchor
				if anchor == "" {
					anchor = pair.Source
				}
				if anchor != strings.TrimSpace(parts[i]) {
					t.Fatal("changed original PDF anchor", pair)
				}
				targets.WriteString(pair.Target)
			}
			for _, formula := range tt.formulas {
				if !strings.Contains(targets.String(), "$"+formula+"$") {
					t.Fatal("formula not restored", formula, items[0])
				}
			}
			stored, err := s.readLayout("doc")
			if err != nil || stored.Blocks[0].Text != tt.source || paragraph.Source != tt.source {
				t.Fatal("rewrote original source", stored, err)
			}
		})
	}
}

func TestTranslationMathRecoverySkipsAreDiagnosed(t *testing.T) {
	const privateText = "PRIVATE_MATH_SOURCE_AND_MODEL_OUTPUT"
	for _, reason := range []string{"block_not_found", "image_asset", "source_mismatch", "no_formulas"} {
		t.Run(reason, func(t *testing.T) {
			s, _, m := translationFixture(t)
			m.Blocks = m.Blocks[:1]
			m.Blocks[0].Text = privateText
			paragraph := m.translationParagraphs()[0]
			switch reason {
			case "block_not_found":
				m.Blocks = nil
			case "image_asset":
				m.Blocks[0].Image = "private-image.png"
			case "source_mismatch":
				m.Blocks[0].Text += " changed"
			}
			logs, err := diagnostics.New(t.TempDir(), "math-recovery-test")
			if err != nil {
				t.Fatal(err)
			}
			s.SetDiagnostics(logs)
			// A skipped recovery returns before rendering or calling a provider.
			if _, err := s.recoverTranslationMath(t.Context(), Document{ID: "doc"}, m, paragraph, nil); !errors.Is(err, errTranslationMath) {
				t.Fatal("changed skipped recovery behavior", err)
			}
			files, err := filepath.Glob(filepath.Join(logs.Dir, "*.jsonl"))
			if err != nil || len(files) != 1 {
				t.Fatal(files, err)
			}
			data, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var event map[string]any
			if err := json.Unmarshal(data, &event); err != nil {
				t.Fatal(err)
			}
			if event["event"] != "translation.math_recovery.skipped" || event["level"] != "warn" || event["documentId"] != "doc" || event["target"] != paragraph.BlockID || event["reasonCode"] != reason {
				t.Fatal("skipped recovery lost identifiers or stable reason", event)
			}
			if bytes.Contains(data, []byte(privateText)) || bytes.Contains(data, []byte("private-image")) {
				t.Fatal("source or image path entered persistent diagnostics")
			}
			var bundle bytes.Buffer
			if err := logs.Export(&bundle, s.Store.Root, nil, nil); err != nil {
				t.Fatal(err)
			}
			archive, err := zip.NewReader(bytes.NewReader(bundle.Bytes()), int64(bundle.Len()))
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range archive.File {
				reader, err := file.Open()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(reader)
				reader.Close()
				if err != nil || bytes.Contains(data, []byte(privateText)) || bytes.Contains(data, []byte("private-image")) {
					t.Fatal("source or image path entered exported diagnostics", file.Name, err)
				}
			}
		})
	}
}
