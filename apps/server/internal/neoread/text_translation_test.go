package neoread

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func translationFixture(t *testing.T) (*Server, Processing, layoutManifest) {
	t.Helper()
	s, p := processingFixture(t)
	s.Token = "test-secret"
	m, _ := s.readLayout("doc")
	b := m.Blocks[0]
	b.Image = ""
	b.Label = "text"
	b.Text = "First sentence. Second sentence."
	b2 := b
	b2.ID = "p1-b2"
	b2.Text = "Another paragraph."
	m.Blocks = []PDFBlock{b, b2}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(s.analysisDir("doc"), "manifest.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.learnPDF(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	return s, p, m
}
func TestTranslationRejectsMissingUnrelatedOrEmptySentences(t *testing.T) {
	cases := []string{
		`{"sentences":[{"source":"First sentence.","target":"第一句。"}]}`,
		`{"sentences":[{"source":"Unrelated words here.","target":"无关。"}]}`,
		`{"sentences":[{"source":"First sentence. Second sentence.","target":""}]}`,
		`{"sentences":[`,
	}
	for _, raw := range cases {
		if _, err := parseTranslation(raw, "First sentence. Second sentence."); err == nil {
			t.Fatalf("accepted invalid alignment: %s", raw)
		}
	}
	sentences, err := parseTranslation(`{"sentences":[{"source":"First sentence.","target":"第一句。"},{"source":"Second sentence.","target":"第二句。"}]}`, "First sentence. Second sentence.")
	if err != nil || len(sentences) != 2 {
		t.Fatalf("valid sentences rejected: %v", err)
	}
}
func TestTranslationPartialFailureAndRetryPreservesCompletedBlocks(t *testing.T) {
	s, p, m := translationFixture(t)
	if p.Phase != "translating" {
		t.Fatal("text-only paper skipped translation")
	}
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		mismatched := strings.Replace(translationLine(m.Blocks[1]), "Another paragraph.", "Unrelated words here.", 1)
		text := translationLine(m.Blocks[1])
		switch n {
		case 1:
			text = translationLine(m.Blocks[0]) + mismatched
		case 2: // The automatic follow-up asks for the rejected paragraph only.
			if batch := readTranslationInput(t, r).Batch; len(batch.Paragraphs) != 1 || batch.Paragraphs[0].BlockID != m.Blocks[1].ID {
				t.Errorf("follow-up regenerated completed text: %+v", batch.Paragraphs)
			}
			text = mismatched
		}

		writeAPIReply(w, text)
	}))
	defer provider.Close()
	c := AIConfig{Primary: "codex", Models: map[string]string{}, TextAPI: APIConnection{URL: provider.URL, Model: "test"}, Capabilities: map[string]Capability{}}
	c.Capabilities["text-api"] = Capability{Text: true, Fingerprint: configPrint(c, "text-api")}
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	if err := s.processTranslation(context.Background(), &p); err == nil {
		t.Fatal("invalid translation reported success")
	}
	if p.Phase != "translating" || p.TranslationsDone != 1 || p.TranslationsTotal != 2 {
		t.Fatalf("partial failure lost translation progress: %+v", p)
	}
	response := request(t, s, "GET", "/api/documents/doc/translations", nil)
	var items []TranslationBlock
	if json.Unmarshal(response.Body.Bytes(), &items) != nil || len(items) != 2 || items[0].Status != "complete" || items[1].Status != "failed" {
		t.Fatalf("partial progress lost: %s", response.Body.String())
	}
	p.Status = "failed"
	if err := s.Store.saveProcessing(p); err != nil {
		t.Fatal(err)
	}
	response = request(t, s, "POST", "/api/documents/doc/translations", strings.NewReader(`{"blockId":"p1-b2"}`))
	if response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	p, _ = s.Store.processing("doc")
	if err := s.processTranslation(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || p.Status != "complete" {
		t.Fatalf("completed paragraph regenerated or job not complete: calls=%d %+v", calls.Load(), p)
	}
	// A changed source must never silently reuse the old translated text.
	m.Blocks[0].Text = "Changed source."
	items, err := s.translations("doc", m)
	if err != nil || items[0].Status != "pending" || len(items[0].Sentences) != 0 || items[1].Status != "complete" {
		t.Fatalf("stale source reused: %+v %v", items, err)
	}
}
func TestTranslationResumesInterruptedWithoutPrioritizingRequestedParagraph(t *testing.T) {
	s, p, m := translationFixture(t)
	first := newTranslation(m.Blocks[0])
	first.Status = "running"
	_ = s.saveTranslation("doc", first)
	response := request(t, s, "POST", "/api/documents/doc/translations", strings.NewReader(`{"blockId":"p1-b2"}`))
	if response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	var order []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batch := readTranslationInput(t, r).Batch
		var out strings.Builder
		for _, paragraph := range batch.Paragraphs {
			order = append(order, paragraph.BlockID)
			out.WriteString(translationParagraphLine(paragraph))
		}
		writeAPIReply(w, out.String())
	}))
	defer provider.Close()
	configureTranslationTest(t, s, provider.URL)
	if err := s.processTranslation(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != m.Blocks[0].ID {
		t.Fatalf("unexpected order: %v", order)
	}
}

func TestTranslationStageResumesPersistedProgressAfterRestart(t *testing.T) {
	for _, phase := range []string{"settling", "translating"} {
		t.Run(phase, func(t *testing.T) {
			s, p, m := translationFixture(t)
			if _, err := s.Store.DB.Exec("UPDATE documents SET classification_status='done'"); err != nil {
				t.Fatal(err)
			}
			first := newTranslation(m.Blocks[0])
			first.Status = "complete"
			first.Sentences = []TranslationSentence{{Source: m.Blocks[0].Text, Target: "已保存译文"}}
			second := newTranslation(m.Blocks[1])
			second.Status = "running"
			for _, item := range []TranslationBlock{first, second} {
				if err := s.saveTranslation("doc", item); err != nil {
					t.Fatal(err)
				}
			}
			p.Phase, p.Status = phase, "running"
			p.TranslationsDone, p.TranslationsTotal = 0, 0
			if err := s.Store.saveProcessing(p); err != nil {
				t.Fatal(err)
			}
			started := make(chan string, 1)
			release := make(chan struct{})
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				batch := readTranslationInput(t, r).Batch
				if len(batch.Paragraphs) != 1 {
					t.Error("regenerated completed translation")
					return
				}
				started <- batch.Paragraphs[0].BlockID
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				sendTranslationDelta(w, translationParagraphLine(batch.Paragraphs[0]))
				finishTranslationStream(w)
			}))
			defer provider.Close()
			configureTranslationTest(t, s, provider.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stop := s.StartProcessing(ctx)
			defer stop()
			if id := waitTranslationSignal(t, started); id != second.BlockID {
				t.Fatalf("wrong pending paragraph: %s", id)
			}
			saved, err := s.Store.processing("doc")
			if err != nil || saved.Phase != "translating" || saved.TranslationsDone != 1 || saved.TranslationsTotal != 2 {
				t.Fatalf("did not restore progress from saved translations: %+v, %v", saved, err)
			}
			close(release)
			deadline := time.Now().Add(10 * time.Second)
			for {
				saved, err = s.Store.processing("doc")
				if err == nil && saved.Phase == "ready" && saved.Status == "complete" && saved.TranslationsDone == 2 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("translation stage did not finish: %+v, %v", saved, err)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestTranslationStageWaitsForTextCapabilityAndResumes(t *testing.T) {
	s, p, _ := translationFixture(t)
	if err := s.processTranslation(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Phase != "translating" || p.Status != "waiting" || p.TranslationsDone != 0 || p.TranslationsTotal != 2 {
		t.Fatalf("wrong translation waiting state: %+v", p)
	}
	s.wakeProcessing()
	saved, err := s.Store.processing("doc")
	if err != nil || saved.Phase != "translating" || saved.Status != "queued" || saved.TranslationsTotal != 2 {
		t.Fatalf("configuration wake lost translation stage: %+v, %v", saved, err)
	}
}

func TestAlgorithmImagesNeverEnterTranslationQueue(t *testing.T) {
	for _, image := range []string{"", "assets/p7-b9.png"} {
		b := PDFBlock{ID: "p7-b9", Page: 7, Label: "algorithm", Text: "Algorithm 1: Read phase\n1 Function read(txn, record)", Image: image}
		m := layoutManifest{Blocks: []PDFBlock{b}}
		if len(m.translationParagraphs()) != 0 || len(m.translationItems()) != 0 {
			t.Fatalf("algorithm queued for translation: %+v", b)
		}
	}
}

func TestAlgorithmCaptionUsesImageTranslationRules(t *testing.T) {
	b := PDFBlock{ID: "p7-b9", Page: 7, Label: "algorithm", Text: "Function read(txn, record)", Caption: "Algorithm 1: Read phase", Image: "assets/p7-b9.png"}
	if translationSource(b) != b.Caption {
		t.Fatal("algorithm did not use the image caption path")
	}
}

func configureTranslationTest(t *testing.T, s *Server, url string) {
	t.Helper()
	c := AIConfig{Primary: "codex", Models: map[string]string{}, TextAPI: APIConnection{URL: url, Model: "test"}, Capabilities: map[string]Capability{}}
	c.Capabilities["text-api"] = Capability{Text: true, Fingerprint: configPrint(c, "text-api")}
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
}
func translationParagraphLine(p translationParagraph) string {
	pairs := []TranslationSentence{}
	for _, source := range translationSentenceParts(p.Source) {
		pairs = append(pairs, TranslationSentence{Source: strings.TrimSpace(source), Target: "译文。"})
	}
	data, _ := json.Marshal(map[string]any{"blockId": p.BlockID, "sourceHash": p.SourceHash, "sentences": pairs})
	return string(data) + "\n"
}
func translationLine(b PDFBlock) string {
	return translationParagraphLine(translationParagraph{b.ID, translationHash(translationSource(b)), translationSource(b)})
}

type translationTestInput struct {
	Title       string           `json:"title"`
	Author      string           `json:"author"`
	FrontMatter []string         `json:"frontMatter"`
	Batch       translationBatch `json:"batch"`
}

func readTranslationInput(t *testing.T, r *http.Request) translationTestInput {
	t.Helper()
	var request struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Error(err)
		return translationTestInput{}
	}
	prompt := request.Messages[len(request.Messages)-1].Content
	_, raw, ok := strings.Cut(prompt, "输入资料：\n")
	if !ok {
		t.Error("missing batch input")
		return translationTestInput{}
	}
	var input translationTestInput
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		t.Error(err)
	}
	return input
}
func TestTranslationBatchBoundariesAndContext(t *testing.T) {
	m := layoutManifest{}
	for i, source := range []string{"一二三四五六七", "八九十甲乙丙丁", "第三段", "第四段", "第五段"} {
		m.Blocks = append(m.Blocks, PDFBlock{ID: fmt.Sprintf("p1-b%d", i+1), Page: 1, Label: "text", Text: source})
	}
	items := []TranslationBlock{}
	for _, b := range m.Blocks {
		items = append(items, newTranslation(b))
	}
	batches := translationBatches(m, items, 10)
	if len(batches) != 2 || len(batches[0].Paragraphs) != 2 || batches[0].Paragraphs[1].Source != "八九十甲乙丙丁" {
		t.Fatalf("split paragraph or counted UTF-8 bytes: %+v", batches)
	}
	if len(batches[0].After) != 2 || len(batches[1].Before) != 2 {
		t.Fatalf("missing context: %+v", batches)
	}
	items[0].Status = "complete"
	retry := translationBatches(m, items, 10)
	if len(retry[0].Paragraphs) != 1 || retry[0].Paragraphs[0].BlockID != m.Blocks[1].ID || retry[0].After[0].BlockID != m.Blocks[2].ID {
		t.Fatal("retry changed boundaries or regenerated completed text")
	}
	prompt := translationPrompt(Document{Title: "论文标题", Author: "作者"}, m, batches[0], "")
	if !strings.Contains(prompt, `"title":"论文标题"`) || !strings.Contains(prompt, `"author":"作者"`) {
		t.Fatal("metadata not attached")
	}
}

func TestRetryLegacySentenceMappingQueuesOnlyThisParagraphAndKeepsVisibleProse(t *testing.T) {
	s, _, m := translationFixture(t)
	old := newTranslation(m.Blocks[0])
	old.Status = "complete"
	old.Sentences = []TranslationSentence{{Source: m.Blocks[0].Text, Target: "旧的一段译文。"}}
	if err := s.saveTranslation("doc", old); err != nil {
		t.Fatal(err)
	}
	other := newTranslation(m.Blocks[1])
	other.Status = "complete"
	other.Sentences = []TranslationSentence{{Source: m.Blocks[1].Text, Target: "另一段保留。"}}
	if err := s.saveTranslation("doc", other); err != nil {
		t.Fatal(err)
	}
	response := request(t, s, "POST", "/api/documents/doc/translations", strings.NewReader(`{"blockId":"p1-b1"}`))
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	states, err := s.translationStates("doc", m)
	if err != nil || states[0].Status != "pending" || states[1].Status != "complete" {
		t.Fatal(states, err)
	}
	visible, err := s.visibleTranslations("doc", m)
	if err != nil || visible[0].Sentences[0].Target != "旧的一段译文。" {
		t.Fatal("old prose was cleared before replacement", visible, err)
	}
}

func TestTranslationProgressUsesCurrentSourceStatusWithoutDecodingCachedProse(t *testing.T) {
	s, p, m := translationFixture(t)
	block := newTranslation(m.Blocks[0])
	if _, err := s.Store.DB.Exec("INSERT INTO translations(document_id,block_id,source_hash,status,body) VALUES(?,?,?,?,?)", "doc", block.BlockID, block.SourceHash, "complete", "unreadable old prose"); err != nil {
		t.Fatal(err)
	}
	old := newTranslation(m.Blocks[1])
	if _, err := s.Store.DB.Exec("INSERT INTO translations(document_id,block_id,source_hash,status,body) VALUES(?,?,?,?,?)", "doc", old.BlockID, "old-version", "complete", "old body"); err != nil {
		t.Fatal(err)
	}
	if err := s.refreshTranslationCounts(&p, m); err != nil || p.TranslationsDone != 1 || p.TranslationsTotal != len(m.translationItems()) {
		t.Fatal("progress decoded prose or counted an obsolete source", p, err)
	}
	if _, err := s.visibleTranslations("doc", m); err == nil {
		t.Fatal("reading no longer validates saved translation bodies")
	}
}

func TestTranslationWithoutPrimaryWaitsBeforeStartingFormulaConversion(t *testing.T) {
	s, p, m := translationFixture(t)
	p.lane = "translating"
	if err := s.translateContent(t.Context(), &p, m); err != nil || p.Status != "waiting" {
		t.Fatal("missing library configuration failed the translation", p, err)
	}
	stored, err := s.Store.processing(p.DocumentID)
	if err != nil || stored.Translating == nil || stored.Translating.Status != "waiting" {
		t.Fatal("waiting state was not persisted", stored, err)
	}
}
func TestTranslationJSONLPublishesOnlyCompleteValidatedLines(t *testing.T) {
	p := translationParagraph{"p1-b1", "version", "Sentence."}
	var saved []TranslationBlock
	d := newTranslationJSONL(translationBatch{Paragraphs: []translationParagraph{p}}, func(b TranslationBlock) error { saved = append(saved, b); return nil })
	line := translationParagraphLine(p)
	for _, chunk := range []string{`{"oops":`, "\n", strings.Replace(line, "version", "wrong", 1), strings.Replace(line, "p1-b1", "p1-b9", 1), line[:len(line)-1]} {
		if err := d.feed(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if len(saved) != 0 {
		t.Fatal("published invalid or incomplete line")
	}
	if err := d.feed("\n" + line); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].BlockID != p.BlockID {
		t.Fatalf("line not published immediately or duplicate overwritten: %+v", saved)
	}
}
func sendTranslationDelta(w http.ResponseWriter, text string) {
	data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": text}}}})
	fmt.Fprintf(w, "data: %s\n\n", data)
	w.(http.Flusher).Flush()
}
func finishTranslationStream(w http.ResponseWriter) {
	fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	w.(http.Flusher).Flush()
}
func waitTranslationSignal(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
		return ""
	}
}
func TestTranslationRunsThreeBatchesAndSavesBeforeProviderCompletes(t *testing.T) {
	s, p, m := translationFixture(t)
	m.Blocks = append(m.Blocks, m.Blocks[1], m.Blocks[1])
	m.Blocks[2].ID = "p1-b3"
	m.Blocks[3].ID = "p1-b4"
	for i := range m.Blocks {
		m.Blocks[i].Text = strings.Repeat(fmt.Sprintf("段%d", i), 5001)
	}
	started := make(chan string, 4)
	released := make(chan struct{})
	defer close(released)
	var active, maxActive atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batch := readTranslationInput(t, r).Batch
		n := active.Add(1)
		defer active.Add(-1)
		for old := maxActive.Load(); n > old; old = maxActive.Load() {
			if maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		started <- batch.Paragraphs[0].BlockID
		w.Header().Set("Content-Type", "text/event-stream")
		sendTranslationDelta(w, translationParagraphLine(batch.Paragraphs[0]))
		select {
		case <-released:
		case <-r.Context().Done():
			return
		}
		finishTranslationStream(w)
	}))
	// Close release before the HTTP server so blocked handlers cannot hang cleanup.
	t.Cleanup(provider.Close)
	configureTranslationTest(t, s, provider.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.settleTranslations(ctx, &p, m) }()
	first, second, third := waitTranslationSignal(t, started), waitTranslationSignal(t, started), waitTranslationSignal(t, started)
	if first == second || first == third || second == third || first == "p1-b4" || second == "p1-b4" || third == "p1-b4" {
		t.Fatalf("bad batch scheduling: %s %s %s", first, second, third)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		items, err := s.translations("doc", m)
		if err != nil {
			t.Fatal(err)
		}
		if items[0].Status == "complete" && items[1].Status == "complete" && items[2].Status == "complete" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("paragraphs waited for provider completion")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-started:
		t.Fatal("started a fourth concurrent batch")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("workers ignored cancellation")
	}
	items, _ := s.translations("doc", m)
	if maxActive.Load() != 3 || items[0].Status != "complete" || items[1].Status != "complete" || items[2].Status != "complete" || items[3].Status != "pending" {
		t.Fatalf("lost partial results or exceeded concurrency: max=%d %+v", maxActive.Load(), items)
	}
	saved, err := s.Store.processing("doc")
	if err != nil || saved.Phase != "translating" || saved.TranslationsDone != 3 || saved.TranslationsTotal != 4 {
		t.Fatalf("streamed progress was not persisted: %+v, %v", saved, err)
	}
}
func TestTranslationSubscriptionSnapshotAndLiveLine(t *testing.T) {
	s, _, m := translationFixture(t)
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/documents/doc/translations/stream", nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	scan := bufio.NewScanner(res.Body)
	next := func() string {
		t.Helper()
		for scan.Scan() {
			if strings.HasPrefix(scan.Text(), "data: ") {
				return strings.TrimPrefix(scan.Text(), "data: ")
			}
		}
		t.Fatalf("stream ended: %v", scan.Err())
		return ""
	}
	if !strings.Contains(next(), `"pending"`) {
		t.Fatal("missing initial snapshot")
	}
	t1 := newTranslation(m.Blocks[0])
	t1.Status = "complete"
	t1.Sentences = []TranslationSentence{{Source: m.Blocks[0].Text, Target: "译文"}}
	if err = s.saveTranslation("doc", t1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(next(), `"complete"`) {
		t.Fatal("saved line not pushed")
	}
	cancel()
	res.Body.Close()
	req, _ = http.NewRequest("GET", server.URL+"/api/documents/doc/translations/stream", nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	scan = bufio.NewScanner(res.Body)
	if !strings.Contains(next(), `"complete"`) {
		t.Fatal("reconnect lost committed paragraph")
	}
}
func TestTranslationTruncatedBatchRetainsRowsAndRetriesOnlyMissing(t *testing.T) {
	s, p, m := translationFixture(t)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batch := readTranslationInput(t, r).Batch
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			sendTranslationDelta(w, translationParagraphLine(batch.Paragraphs[0]))
			tail := translationParagraphLine(batch.Paragraphs[1])
			sendTranslationDelta(w, tail[:len(tail)/2])
			return // Upstream disconnects without a successful terminal event.
		}
		if len(batch.Paragraphs) != 1 || batch.Paragraphs[0].BlockID != m.Blocks[1].ID {
			t.Errorf("regenerated completed row: %+v", batch.Paragraphs)
		}
		sendTranslationDelta(w, translationParagraphLine(batch.Paragraphs[0]))
		finishTranslationStream(w)
	}))
	defer provider.Close()
	configureTranslationTest(t, s, provider.URL)
	if err := s.settleTranslations(context.Background(), &p, m); err == nil {
		t.Fatal("truncated batch reported complete")
	}
	items, _ := s.translations("doc", m)
	if items[0].Status != "complete" || items[1].Status != "failed" {
		t.Fatalf("partial progress lost: %+v", items)
	}
	p.Status = "failed"
	_ = s.Store.saveProcessing(p)
	if res := request(t, s, "POST", "/api/documents/doc/translations", strings.NewReader(`{"blockId":"p1-b2"}`)); res.Code != 202 {
		t.Fatal(res.Body.String())
	}
	configureTranslationTest(t, s, provider.URL)
	if err := s.settleTranslations(context.Background(), &p, m); err != nil {
		t.Fatal(err)
	}
	items, _ = s.translations("doc", m)
	if calls.Load() != 2 || items[0].Status != "complete" || items[1].Status != "complete" {
		t.Fatal("resume failed")
	}
}
func TestTranslationDeadlineDoesNotChangeChatDeadline(t *testing.T) {
	for _, timeout := range []time.Duration{0, translationBatchTimeout} {
		service := &GenerationService{primary: "codex", timeout: timeout, connections: map[string]generationConnection{}}
		service.connections["codex"] = generationConnection{VerifiedText: true, Adapter: adapterFunc(func(ctx context.Context, _ GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
			deadline, ok := ctx.Deadline()
			want := timeout
			if want == 0 {
				want = 15 * time.Minute
			}
			if !ok || time.Until(deadline) > want || time.Until(deadline) < want-time.Second {
				t.Errorf("unexpected timeout: %v", time.Until(deadline))
			}
			if err := emit(ProviderEvent{Text: "ok"}); err != nil {
				return GenerateResult{}, err
			}
			return GenerateResult{Text: "ok", FinishReason: "stop"}, nil
		})}
		if _, err := service.Generate(context.Background(), AIInput{}, false, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTranslationJSONLRepairsUnescapedLatexBackslashes(t *testing.T) {
	p := translationParagraph{"p1-b2", "h2", "Two."}
	var got TranslationBlock
	d := newTranslationJSONL(translationBatch{Paragraphs: []translationParagraph{p}}, func(block TranslationBlock) error {
		got = block
		return nil
	})
	line := `{"blockId":"p1-b2","sourceHash":"h2","sentences":[{"source":"Two.","target":"$\pi$、$\eta$、$\ll$、$\log n$"}]}`
	if err := d.feed(line + "\n"); err != nil {
		t.Fatal(err)
	}
	if !d.completed[p.BlockID] || len(got.Sentences) != 1 || got.Sentences[0].Target != `$\pi$、$\eta$、$\ll$、$\log n$` {
		t.Fatalf("unescaped LaTeX was not repaired: decoder=%+v block=%+v", d, got)
	}
}

func TestRepairInvalidJSONEscapesPreservesValidEscapesAndOutsideText(t *testing.T) {
	valid := `{"target":"quote: \" newline: \n unicode: \u03c0 slash: \\ solidus: \/"}`
	if got, changed := repairInvalidJSONEscapes(valid); changed || got != valid {
		t.Fatalf("valid JSON changed: changed=%v %q", changed, got)
	}
	broken := `{"target":"$\pi$"}\tail`
	want := `{"target":"$\\pi$"}\tail`
	if got, changed := repairInvalidJSONEscapes(broken); !changed || got != want {
		t.Fatalf("wrong repair: changed=%v got=%q want=%q", changed, got, want)
	}
}

func TestTranslationJSONLNamesStructurallyBrokenLines(t *testing.T) {
	a, b := translationParagraph{"p1-b1", "h1", "One."}, translationParagraph{"p1-b2", "h2", "Two."}
	d := newTranslationJSONL(translationBatch{Paragraphs: []translationParagraph{a, b}}, func(TranslationBlock) error { return nil })
	for _, line := range []string{"```jsonl", `{"blockId":"p1-b2","sourceHash":"h2","sentences":`, translationParagraphLine(a)} {
		if err := d.feed(strings.TrimSuffix(line, "\n") + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.HasPrefix(d.invalid["p1-b2"], "第 2 行不是合法的 JSON") || d.malformed != 1 || !strings.HasPrefix(d.firstMalformed, "第 1 行") || !d.completed["p1-b1"] {
		t.Fatalf("broken lines not attributed: %+v %d %q", d.invalid, d.malformed, d.firstMalformed)
	}
}
func TestTranslationAsksOnceMoreForParagraphsTheModelSkipped(t *testing.T) {
	s, p, m := translationFixture(t)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batch := readTranslationInput(t, r).Batch
		if calls.Add(1) == 2 && (len(batch.Paragraphs) != 1 || batch.Paragraphs[0].BlockID != m.Blocks[1].ID) {
			t.Errorf("follow-up did not ask for the skipped paragraph only: %+v", batch.Paragraphs)
		}
		// The model ends normally after the first paragraph it was given.
		writeAPIReply(w, translationParagraphLine(batch.Paragraphs[0]))
	}))
	defer provider.Close()
	configureTranslationTest(t, s, provider.URL)
	if err := s.settleTranslations(context.Background(), &p, m); err != nil {
		t.Fatal(err)
	}
	items, _ := s.translations("doc", m)
	if calls.Load() != 2 || items[0].Status != "complete" || items[1].Status != "complete" {
		t.Fatalf("skipped paragraph not recovered: calls=%d %+v", calls.Load(), items)
	}
	if _, err := os.Stat(filepath.Join(s.Store.Root, "cache", "doc", "translation-failures")); !os.IsNotExist(err) {
		t.Fatal("recorded a failure for a recovered batch")
	}
}
func TestTranslationFailureNamesTheCauseAndKeepsTheOutput(t *testing.T) {
	s, p, m := translationFixture(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batch := readTranslationInput(t, r).Batch
		text := "not json at all\n"
		if len(batch.Paragraphs) == 2 {
			text = translationParagraphLine(batch.Paragraphs[0])
		}
		writeAPIReply(w, text)
	}))
	defer provider.Close()
	configureTranslationTest(t, s, provider.URL)
	if err := s.settleTranslations(context.Background(), &p, m); err == nil {
		t.Fatal("unrecovered paragraph reported complete")
	}
	items, _ := s.translations("doc", m)
	want := "补译后仍未完成：模型只返回了 0/1 段，另有 1 行无法解析（第 1 行不是合法的 JSON"
	if items[0].Status != "complete" || items[1].Status != "failed" || !strings.HasPrefix(items[1].Error, want) {
		t.Fatalf("failure cause not recorded: %+v", items[1])
	}
	dir := filepath.Join(s.Store.Root, "cache", "doc", "translation-failures")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("diagnostic not written: %v %v", entries, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	cut := strings.Index(string(data), "--- raw output ---")
	if cut < 0 || !strings.Contains(string(data)[:cut], "=== attempt 2:") {
		t.Fatal("trusted retry statistics must precede all model output")
	}
	for _, part := range []string{"batch: p1-b1…p1-b2 (2 paragraphs)", "failed p1-b2: " + want, "=== attempt 2: 1 paragraphs requested, 0 saved, 1 output lines, 1 unparsable", "not json at all"} {
		if !strings.Contains(string(data), part) {
			t.Fatalf("diagnostic misses %q:\n%s", part, data)
		}
	}
	// Only the most recent diagnostics are kept.
	batch := translationBatch{Paragraphs: []translationParagraph{{BlockID: "p1-b2"}}}
	attempt := &translationAttempt{decoder: newTranslationJSONL(batch, nil)}
	for i := 0; i < translationFailureLogs+2; i++ {
		s.recordTranslationFailure("doc", batch, []*translationAttempt{attempt}, []TranslationBlock{{BlockID: "p1-b2", Error: "x"}})
	}
	if entries, _ = os.ReadDir(dir); len(entries) != translationFailureLogs {
		t.Fatalf("kept %d diagnostics", len(entries))
	}
}

func TestTranslationServiceKeepsOtherProviderTaskSettings(t *testing.T) {
	s, _, _ := translationFixture(t)
	config := AIConfig{
		Primary:            "claude",
		Models:             map[string]string{"claude": "chat-model", "codex": "chat-codex"},
		TranslationModels:  map[string]string{"claude": "translation-model", "codex": "selected-codex"},
		TranslationEfforts: map[string]map[string]string{"claude": {"translation-model": "low"}},
	}
	service, adapter := s.translationService(config)
	defer adapter.close()
	claude, ok := service.connections["claude"].Adapter.(configuredCLIAdapter)
	if !ok || service.primary != "claude" || claude.model != "translation-model" || claude.level != "low" || claude.task != taskTranslation {
		t.Fatalf("translation task settings not preserved: %#v", service)
	}
	if service.connections["codex"].Adapter != adapter.codex || config.Models["codex"] != "chat-codex" || config.TranslationModels["codex"] != "selected-codex" {
		t.Fatal("Codex adapter or saved model settings changed")
	}
}

func runTranslationSessionProcess() {
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), maxProviderFrame)
	enc := json.NewEncoder(os.Stdout)
	turn := 0
	model := ""
	for scan.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &req) != nil {
			os.Exit(1)
		}
		f, err := os.OpenFile(os.Getenv("NEOREAD_TRANSLATION_CAPTURE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(2)
		}
		_ = json.NewEncoder(f).Encode(map[string]any{"pid": os.Getpid(), "method": req.Method, "params": req.Params})
		_ = f.Close()
		reply := func(v any) { _ = enc.Encode(map[string]any{"id": req.ID, "result": v}) }
		switch req.Method {
		case "config/read":
			reply(map[string]any{"config": map[string]any{"mcp_servers": map[string]any{}, "features": map[string]bool{}, "web_search": "disabled"}})
		case "initialize":
			reply(map[string]any{})
		case "thread/start":
			var params struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(req.Params, &params)
			model = params.Model
			actual := model
			if actual == "" {
				actual = "cli-default"
			}
			reply(map[string]any{"thread": map[string]string{"id": "thread"}, "model": actual})
		case "turn/start":
			turn++
			tid := fmt.Sprintf("turn-%d", turn)
			reply(map[string]any{"turn": map[string]string{"id": tid}})
			notify := func(method string, p map[string]any) {
				p["threadId"] = "thread"
				p["turnId"] = tid
				_ = enc.Encode(map[string]any{"method": method, "params": p})
			}
			var params struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			_ = json.Unmarshal(req.Params, &params)
			_, raw, _ := strings.Cut(params.Input[0].Text, "输入资料：\n")
			var input translationTestInput
			if json.Unmarshal([]byte(raw), &input) != nil {
				os.Exit(3)
			}
			if turn > 1 {
				_ = enc.Encode(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread", "turnId": fmt.Sprintf("turn-%d", turn-1), "itemId": "old", "delta": "stale\n"}})
			}
			notify("thread/tokenUsage/updated", map[string]any{"tokenUsage": map[string]any{"total": TokenCounts{InputTokens: int64(turn * 20), OutputTokens: int64(turn * 10), TotalTokens: int64(turn * 30), CachedInputTokens: tokenCount(int64(turn * 8))}}})
			if os.Getenv("NEOREAD_TRANSLATION_FAIL_SECOND") == "1" && turn == 2 || os.Getenv("NEOREAD_TRANSLATION_FAIL_MODEL") == model && model != "" {
				notify("turn/completed", map[string]any{"turn": map[string]string{"id": tid, "status": "failed"}})
				continue
			}
			text := ""
			for _, p := range input.Batch.Paragraphs {
				text += translationParagraphLine(p)
			}
			notify("item/agentMessage/delta", map[string]any{"itemId": "answer", "delta": text})
			if os.Getenv("NEOREAD_TRANSLATION_CANCEL") == "1" {
				if !scan.Scan() {
					return
				}
				var interrupt struct {
					Method string `json:"method"`
				}
				_ = json.Unmarshal(scan.Bytes(), &interrupt)
				if interrupt.Method != "turn/interrupt" {
					os.Exit(4)
				}
				_ = os.WriteFile(os.Getenv("NEOREAD_AGENT_ACK"), []byte("interrupt"), 0600)
				return
			}
			notify("item/completed", map[string]any{"item": map[string]string{"id": "answer", "type": "agentMessage", "text": text}})
			notify("turn/completed", map[string]any{"turn": map[string]string{"id": tid, "status": "completed"}})
		}
	}
}

func TestTranslationCodexUsesMainAutomaticSelectionAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%v", fallback), func(t *testing.T) {
			s, _, m, capture := translationSessionFixture(t)
			config := s.aiConfig()
			config.TranslationModels = nil
			config.TranslationEfforts = nil
			s.modelCache = map[string]modelCatalogEntry{"codex": {
				models:  []AgentModel{{ID: "gpt-6-luna", SupportedEfforts: []string{"low", "medium", "high"}}, {ID: "gpt-5.6-luna"}},
				expires: time.Now().Add(time.Minute),
			}}
			if fallback {
				t.Setenv("NEOREAD_TRANSLATION_FAIL_MODEL", "gpt-6-luna")
			}
			service, adapter := s.translationService(config)
			defer adapter.close()
			batch := translationBatch{Paragraphs: []translationParagraph{{BlockID: m.Blocks[0].ID, SourceHash: translationHash(m.Blocks[0].Text), Source: m.Blocks[0].Text}}}
			if _, err := service.Generate(t.Context(), AIInput{Prompt: translationPrompt(Document{}, m, batch, "")}, false, nil); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			var selected []string
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var row struct {
					Method string `json:"method"`
					Params struct {
						Model  string `json:"model"`
						Effort string `json:"effort"`
					} `json:"params"`
				}
				if err := json.Unmarshal([]byte(line), &row); err != nil {
					t.Fatal(err)
				}
				if row.Method == "thread/start" {
					selected = append(selected, row.Params.Model)
				}
				if row.Method == "turn/start" && row.Params.Effort != "medium" {
					t.Fatalf("did not use default effort: %+v", row)
				}
			}
			if len(selected) == 0 || selected[0] != "gpt-6-luna" {
				t.Fatalf("wrong automatic model: %v", selected)
			}
			if fallback && (len(selected) != 2 || selected[1] != "") || !fallback && len(selected) != 1 {
				t.Fatalf("wrong fallback: %v", selected)
			}
		})
	}
}
func translationSessionFixture(t *testing.T) (*Server, Processing, layoutManifest, string) {
	t.Helper()
	fakeAgent(t, "codex", "translation-session")
	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	t.Setenv("NEOREAD_TRANSLATION_CAPTURE", capture)
	s, p, m := translationFixture(t)
	s.modelCache = map[string]modelCatalogEntry{"codex": {models: []AgentModel{{ID: "selected-codex", SupportedEfforts: []string{"low", "medium", "high"}}}, expires: time.Now().Add(time.Minute)}}
	for i := range m.Blocks {
		m.Blocks[i].Text = strings.Repeat(fmt.Sprintf("Paragraph %d. ", i), 1000)
	}
	c := AIConfig{Primary: "codex", Models: map[string]string{"codex": "chat-model"}, TranslationModels: map[string]string{"codex": "selected-codex"}, TranslationEfforts: map[string]map[string]string{"codex": {"selected-codex": "high"}}, Capabilities: map[string]Capability{}}
	c.Capabilities["codex"] = Capability{Text: true, Fingerprint: configPrint(c, "codex")}
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	return s, p, m, capture
}
func TestTranslationCodexWorkersReuseSelectedModelAndEffort(t *testing.T) {
	s, p, m, capture := translationSessionFixture(t)
	for i := 2; i < 6; i++ {
		b := m.Blocks[0]
		b.ID = fmt.Sprintf("p1-b%d", i+1)
		b.Text = strings.Repeat(fmt.Sprintf("Paragraph %d. ", i), 1000)
		m.Blocks = append(m.Blocks, b)
	}
	if err := s.settleTranslations(t.Context(), &p, m); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	type counts struct{ starts, initializes, turns int }
	processes := map[int]*counts{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r struct {
			PID    int    `json:"pid"`
			Method string `json:"method"`
			Params struct {
				Model    string `json:"model"`
				Effort   string `json:"effort"`
				ThreadID string `json:"threadId"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if processes[r.PID] == nil {
			processes[r.PID] = &counts{}
		}
		count := processes[r.PID]
		switch r.Method {
		case "initialize":
			count.initializes++
		case "thread/start":
			count.starts++
			if r.Params.Model != "selected-codex" {
				t.Fatal("wrong model")
			}
		case "turn/start":
			count.turns++
			if r.Params.Effort != "high" || r.Params.ThreadID != "thread" {
				t.Fatal("wrong effort or thread")
			}
		}
	}
	for pid, count := range processes {
		if count.turns == 0 {
			if count.starts != 0 {
				t.Fatal("preflight started a generation thread")
			}
			delete(processes, pid)
		}
	}
	if len(processes) != 3 {
		t.Fatalf("expected three worker processes: %+v", processes)
	}
	turns := 0
	for pid, count := range processes {
		turns += count.turns
		if count.initializes != 1 || count.starts != 1 || count.turns < 1 {
			t.Fatalf("pid=%d counts=%+v", pid, count)
		}
	}
	if turns != 6 {
		t.Fatalf("expected six turns across three reused threads, got %d", turns)
	}
	items, err := s.translations("doc", m)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Status != "complete" {
			t.Fatalf("lost paragraph: %+v", item)
		}
	}
	report, err := s.Store.usageReport("doc")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Calls) != 6 {
		t.Fatalf("calls=%+v", report.Calls)
	}
	for _, call := range report.Calls {
		if len(call.Models) != 1 || call.Models[0].Model != "selected-codex" || call.Models[0].Tokens == nil || call.Models[0].Tokens.TotalTokens != 30 {
			t.Fatalf("usage double-counted: %+v", call)
		}
	}
	if s.aiConfig().Models["codex"] != "chat-model" {
		t.Fatal("changed chat model")
	}
	entries, err := os.ReadDir(filepath.Join(s.Store.Root, "ai-work"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("leaked directory: %v %v", entries, err)
	}
}
func TestTranslationCodexCancellationClosesSession(t *testing.T) {
	s, _, m, _ := translationSessionFixture(t)
	t.Setenv("NEOREAD_TRANSLATION_CANCEL", "1")
	_, sessions := s.translationService(s.aiConfig())
	defer sessions.close()
	a := sessions.codex
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batch := translationBatch{Paragraphs: []translationParagraph{{BlockID: m.Blocks[0].ID, SourceHash: translationHash(m.Blocks[0].Text), Source: m.Blocks[0].Text}}}
	_, err := a.Stream(ctx, GenerateRequest{AIInput{Prompt: translationPrompt(Document{}, m, batch, "")}}, func(e ProviderEvent) error {
		if e.Text != "" {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || a.session != nil || a.work != "" {
		t.Fatalf("cancellation leaked session: %v %+v", err, a)
	}
}

func TestTranslationCodexCancellationClosesAllWorkers(t *testing.T) {
	s, p, m, capture := translationSessionFixture(t)
	b := m.Blocks[0]
	b.ID = "p1-b3"
	m.Blocks = append(m.Blocks, b)
	t.Setenv("NEOREAD_TRANSLATION_CANCEL", "1")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.settleTranslations(ctx, &p, m) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		items, err := s.translations("doc", m)
		if err != nil {
			t.Fatal(err)
		}
		complete := 0
		for _, item := range items {
			if item.Status == "complete" {
				complete++
			}
		}
		if complete == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("three workers did not stream paragraphs")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation reported %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("workers did not stop")
	}
	data, err := os.ReadFile(capture)
	if err != nil || strings.Count(string(data), `"method":"thread/start"`) != 3 {
		t.Fatalf("did not create three threads: %s %v", data, err)
	}
	entries, err := os.ReadDir(filepath.Join(s.Store.Root, "ai-work"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("worker directories leaked: %v %v", entries, err)
	}
}

func TestTranslationCodexFailureRetryPreservesCompletedParagraphs(t *testing.T) {
	s, p, m, capture := translationSessionFixture(t)
	t.Setenv("NEOREAD_TRANSLATION_FAIL_SECOND", "1")
	// Exercise a failed subsequent turn on one worker's reused conversation.
	service, adapter := s.translationService(s.aiConfig())
	defer adapter.close()
	initial, err := s.textTranslations("doc", m)
	if err != nil {
		t.Fatal(err)
	}
	batches := translationBatches(m, initial, translationBatchCharacters)
	if err := s.translateBatch(t.Context(), Document{ID: "doc"}, m, batches[0], service, func() {}); err != nil {
		t.Fatal(err)
	}
	if err := s.translateBatch(t.Context(), Document{ID: "doc"}, m, batches[1], service, func() {}); err != nil {
		t.Fatal(err)
	}
	adapter.close()
	items, err := s.translations("doc", m)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Status != "complete" || items[1].Status != "failed" {
		t.Fatalf("lost completed translation: %+v", items)
	}
	if !capable(s.aiConfig(), "codex", false) {
		t.Fatal("translation failure invalidated chat capability")
	}
	t.Setenv("NEOREAD_TRANSLATION_FAIL_SECOND", "")
	items[1].Status = "pending"
	items[1].Error = ""
	if err := s.saveTranslation("doc", items[1]); err != nil {
		t.Fatal(err)
	}
	if err := s.settleTranslations(t.Context(), &p, m); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	starts, turns := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r struct {
			Method string `json:"method"`
			Params struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.Method == "thread/start" {
			starts++
		}
		if r.Method == "turn/start" {
			turns++
			if turns == 3 {
				_, raw, _ := strings.Cut(r.Params.Input[0].Text, "输入资料：\n")
				var input translationTestInput
				if json.Unmarshal([]byte(raw), &input) != nil || len(input.Batch.Paragraphs) != 1 || input.Batch.Paragraphs[0].BlockID != m.Blocks[1].ID {
					t.Fatal("retry regenerated completed paragraph")
				}
			}
		}
	}
	if starts != 2 || turns != 3 {
		t.Fatalf("starts=%d turns=%d", starts, turns)
	}
}

func sentencesJSON(sources ...string) string {
	sentences := []TranslationSentence{}
	for _, source := range sources {
		for _, part := range translationSentenceParts(source) {
			sentences = append(sentences, TranslationSentence{Source: strings.TrimSpace(part), Target: "译文。"})
		}
	}
	data, _ := json.Marshal(map[string]any{"sentences": sentences})
	return string(data)
}

func compact(s string) string { return strings.Join(strings.Fields(s), "") }

func TestSentenceQuotesStayWithTheSentenceTheyClose(t *testing.T) {
	for _, source := range []string{
		`The query includes “ money laundering service. ” Using an LLM, we continue.`,
		`The query includes "money laundering service. " Using an LLM, we continue.`,
		"The query includes ‘money laundering service.\n’ Using an LLM, we continue.",
		`The query includes “money laundering service. " Using an LLM, we continue.`,
		`The query includes "money laundering service. ” Using an LLM, we continue.`,
	} {
		parts := translationSentenceParts(source)
		if len(parts) != 2 || strings.Contains(strings.TrimSpace(parts[1]), "service") || !strings.HasPrefix(strings.TrimSpace(parts[1]), "Using") {
			t.Fatalf("closing quote moved to the next sentence: %#v", parts)
		}
		if compact(strings.Join(parts, "")) != compact(source) {
			t.Fatal("segmentation lost source text", parts)
		}
	}
	parts := translationSentenceParts(`First sentence. "The next sentence starts a new quote."`)
	if len(parts) != 2 || !strings.HasPrefix(strings.TrimSpace(parts[1]), `"The next`) {
		t.Fatal("opening quote moved backwards", parts)
	}
}

func TestCachedQuoteBoundariesRepairBothSidesWithoutRetranslating(t *testing.T) {
	source := `The query includes “ money laundering service. ” Using an LLM, we continue.`
	old := TranslationBlock{Status: "complete", SourceHash: translationHash(source), Sentences: []TranslationSentence{
		{Source: `The query includes “ money laundering service.`, Target: `查询包括“洗钱服务。`},
		{Source: `” Using an LLM, we continue.`, Target: `”我们使用大语言模型继续。`},
	}}
	current := repairCachedTranslationAnchors(old, source)
	if current.Error != "" || len(current.Sentences) != 2 || !strings.HasSuffix(current.Sentences[0].Source, "”") || !strings.HasSuffix(current.Sentences[0].Target, "”") || strings.HasPrefix(current.Sentences[1].Target, "”") {
		t.Fatal("cached closing quote not repaired", current)
	}
	if current.SourceHash != old.SourceHash || compact(current.Sentences[0].Target+current.Sentences[1].Target) != compact(old.Sentences[0].Target+old.Sentences[1].Target) {
		t.Fatal("cached translation content changed", current)
	}
	if got := repairCachedTranslationAnchors(current, source); jsonText(got) != jsonText(current) {
		t.Fatal("quote repair is not stable", got)
	}
}

func TestSentenceQuoteOnlyTailAndNewOpeningQuoteStayDistinct(t *testing.T) {
	ending := `A “quoted sentence. ”`
	if parts := translationSentenceParts(ending); len(parts) != 1 || compact(parts[0]) != compact(ending) {
		t.Fatal("closing quote became a standalone sentence", parts)
	}
	source := `Readers' examples end here. "Don't move this opening quote."`
	if parts := translationSentenceParts(source); len(parts) != 2 || !strings.HasPrefix(strings.TrimSpace(parts[1]), `"Don't`) {
		t.Fatal("opening quote or apostrophe moved", parts)
	}
	ordinary := []TranslationSentence{{Source: "  Plain sentence. ", Target: "普通译文。"}}
	if got := reattachTranslationQuotes(ordinary); jsonText(got) != jsonText(ordinary) {
		t.Fatal("quote repair changed ordinary prose", got)
	}
}

func TestScientificAbbreviationsDoNotRejectAlreadySentenceWiseTranslations(t *testing.T) {
	for _, source := range []string{
		"This formulation achieves 94.4% success on Qwen-VL (vs. 68.8% baseline) with fewer iterations.",
		"(C) This induces safety blindness, achieving 95.5% ASR vs. 70.0% baseline.",
		"Prompts used for multimodal evidence generation are listed in App. A.",
	} {
		raw := jsonText(map[string]any{"sentences": []TranslationSentence{{Source: source, Target: "这是对应的一个中文句子。"}}})
		pairs, err := parseTranslationSource(raw, source, true)
		if err != nil || len(pairs) != 1 {
			t.Fatalf("abbreviation split a source sentence: %q: %v", source, err)
		}
		cached := repairCachedTranslationAnchors(TranslationBlock{Status: "complete", Sentences: []TranslationSentence{{Source: source, Target: "这是对应的一个中文句子。"}}}, source)
		if cached.Error != "" {
			t.Fatal("valid cached sentence acquired a legacy warning", cached.Error)
		}
	}
}

func TestTranslationPreservesSquareUnionAndFootnoteSymbols(t *testing.T) {
	for _, pair := range [][2]string{
		{`Let $q=\bigsqcup_j q_j$.`, `令 $q=\sum_j q_j$。`},
		{`Author$^{1\ddagger}$`, `Author$^{1\dagger\dagger}$`},
	} {
		if matchingMathDecorations(pair[0], pair[1]) {
			t.Fatal("different mathematical symbols accepted", pair)
		}
	}
	if !matchingMathDecorations(`Let $q=\bigsqcup_j q_j$.`, `令 $q=\sqcup_j q_j$。`) {
		t.Fatal("equivalent square-union notation was rejected")
	}
}

func TestJailBoundLegacyParagraphRestoresSentencePairs(t *testing.T) {
	body, err := os.ReadFile("../../../web/src/readers/fixtures/jailbound-sentence-alignment.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Block     PDFBlock              `json:"block"`
		Sentences []TranslationSentence `json:"sentences"`
	}
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	target := ""
	for _, sentence := range fixture.Sentences {
		target += sentence.Target
	}
	cached := TranslationBlock{BlockID: fixture.Block.ID, SourceHash: translationHash(fixture.Block.Text), Status: "complete", Sentences: []TranslationSentence{{Source: fixture.Block.Text, Target: target}}}
	restored := repairCachedTranslationAnchors(cached, fixture.Block.Text)
	if restored.Error != "" || len(restored.Sentences) != 3 {
		t.Fatal("paragraph remained one pair", restored)
	}
	for i, sentence := range restored.Sentences {
		if sentence.Source != fixture.Sentences[i].Source || sentence.Target != fixture.Sentences[i].Target {
			t.Fatal("incorrect sentence pair", i, sentence)
		}
	}
	if len(cached.Sentences) != 1 {
		t.Fatal("reading modified cached data")
	}
	parsed, err := parseTranslation(jsonText(map[string]any{"sentences": cached.Sentences}), fixture.Block.Text)
	if err != nil || len(parsed) != 3 {
		t.Fatal("fresh reply kept a paragraph pair", parsed, err)
	}
}

func TestSentenceSegmentationKeepsMathAndCitations(t *testing.T) {
	source := "We used the dataset Liu et al. [2024c], with a 0.3 threshold. The next step is unchanged."
	if parts := translationSentenceParts(source); len(parts) != 2 {
		t.Fatal(parts)
	}
	target := `使用 $\\text{Prob. A} = 0.3$ 作为阈值。下一步保持不变。`
	if parts := translationSentenceParts(target); len(parts) != 2 {
		t.Fatal(parts)
	}
	if _, err := translationSentencePairs([]TranslationSentence{{Source: "First sentence. Second sentence.", Target: "合并成一句。"}}); err == nil {
		t.Fatal("guessed an ambiguous sentence mapping")
	}
}

func TestTranslationRejectsLostMeanBarsAndInventedLayerSuperscripts(t *testing.T) {
	source := `The refusal vector is $\mathbf{v}_{\mathrm{refu}}=\mathbf{v}_{\mathrm{text}}=\bar{\mathbf{h}}_{\mathrm{harm}}^{(l)}-\bar{\mathbf{h}}_{\mathrm{safe}}^{(l)}$.`
	good := `拒绝向量是 $\mathbf{v}_{\mathrm{refu}}=\mathbf{v}_{\mathrm{text}}=\bar{\mathbf{h}}_{\mathrm{harm}}^{(l)}-\bar{\mathbf{h}}_{\mathrm{safe}}^{(l)}$。`
	bad := `拒绝向量是 $\mathbf{v}_{\mathrm{refu}}^{(l)}=\mathbf{v}_{\mathrm{text}}^{(l)}=\mathbf{h}_{\mathrm{harm}}^{(l)}-\mathbf{h}_{\mathrm{safe}}^{(l)}$。`
	for _, target := range []string{good, bad} {
		_, err := parseTranslation(jsonText(map[string]any{"sentences": []TranslationSentence{{Source: source, Target: target}}}), source)
		if (err == nil) != (target == good) {
			t.Fatal(target, err)
		}
	}
}

// Extracted from a VLDB paper: math italics with subscripts moved out of place.
const garbledParagraph = "Let 𝑇 be a set of transactions that depend on directly or tran- 𝑖 𝑡 𝑖 sitively, where for all 𝑡 in 𝑇 , there exists a path from 𝑖 𝑡 to in 𝑖 𝑡 𝑀𝑉 𝑆𝐺 ( 𝑠, ≪) through any types of edges, wr , rw , and ww . We refer to 𝑇 as the followers of 𝑡 𝑖."

func TestTranslationAcceptsRepairedExtractionAndAnchorsOriginal(t *testing.T) {
	model := []string{
		"Let T_i be a set of transactions that depend on t_i directly or transitively, where for all t in T_i, there exists a path from t to t_i in MVSG(s, ≪) through any types of edges, wr, rw, and ww.",
		"We refer to T_i as the followers of t_i.",
	}
	sentences, err := parseTranslation(sentencesJSON(model...), garbledParagraph)
	if err != nil {
		t.Fatal(err)
	}
	if sentences[0].Source != model[0] || sentences[1].Source != model[1] {
		t.Fatal("model sentences were not kept")
	}
	joined := sentences[0].Anchor + sentences[1].Anchor
	if compact(joined) != compact(garbledParagraph) {
		t.Fatalf("anchors do not cover the paragraph: %q", joined)
	}
	if !strings.HasPrefix(sentences[1].Anchor, "We refer to") {
		t.Fatalf("second anchor misplaced: %q", sentences[1].Anchor)
	}
}

func TestTranslationRepairsSplitWordsWithAnchors(t *testing.T) {
	source := "The A GENT reads the paper. It writes a sum- mary."
	sentences, err := parseTranslation(sentencesJSON("The AGENT reads the paper.", "It writes a summary."), source)
	if err != nil {
		t.Fatal(err)
	}
	if sentences[0].Anchor != "The A GENT reads the paper." || sentences[1].Anchor != "It writes a sum- mary." {
		t.Fatalf("anchors: %q %q", sentences[0].Anchor, sentences[1].Anchor)
	}
}

func TestExactTranslationNeedsNoAnchors(t *testing.T) {
	sentences, err := parseTranslation(sentencesJSON("First sentence.", "Second sentence."), "First sentence.  Second sentence.")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sentences {
		if s.Anchor != "" {
			t.Fatalf("exact sentence got an anchor: %+v", s)
		}
	}
}

func TestTranslationRejectsASentenceWithoutAnOriginalAnchor(t *testing.T) {
	source := "The attention mechanism connects the positions in the network and reduces the number of sequential operations needed to process the input."
	if _, err := parseTranslation(sentencesJSON(source, "XYZ XYZ XYZ."), source); err == nil {
		t.Fatal("accepted a translated sentence with no original source span")
	}
}

func TestCachedTranslationRepairsMissingAnchorsWithoutChangingSavedText(t *testing.T) {
	source := "The A GENT reads the paper. It writes a sum- mary."
	store := archiveFixture(t, t.TempDir())
	archiveDocument(t, store, "paper", "分类")
	s := NewServer(store, "test-token", "")
	t.Cleanup(s.Close)
	m := layoutManifest{Blocks: []PDFBlock{{ID: "p1-b1", Page: 1, Label: "text", Text: source}}}
	block := TranslationBlock{BlockID: "p1-b1", SourceHash: translationHash(source), Status: "complete", Sentences: []TranslationSentence{{Source: "The AGENT reads the paper.", Target: "第一句。"}, {Source: "It writes a summary.", Target: "第二句。"}}}
	if err := s.saveTranslation("paper", block); err != nil {
		t.Fatal(err)
	}
	got, err := s.translations("paper", m)
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	if got[0].Sentences[0].Anchor != "The A GENT reads the paper." || got[0].Sentences[1].Anchor != "It writes a sum- mary." {
		t.Fatal("legacy translation did not receive original sentence anchors", got)
	}
	if got[0].Sentences[0].Source != block.Sentences[0].Source || got[0].Sentences[1].Target != block.Sentences[1].Target {
		t.Fatal("saved prose changed", got)
	}
	if indexes := recoverySentenceIndexes(got[0], block.Sentences[1].Source); len(indexes) != 1 || indexes[0] != 1 {
		t.Fatal("existing annotations lost their sentence after restoring anchors", indexes)
	}
	var body string
	if err := store.DB.QueryRow("SELECT body FROM translations WHERE document_id='paper' AND block_id=? AND source_hash=?", block.BlockID, block.SourceHash).Scan(&body); err != nil || body != jsonText(block) {
		t.Fatal("reading rewrote the saved translation", body, err)
	}
}

func TestTranslationRejectsDroppedHalf(t *testing.T) {
	source := "Transactions commit in order. Readers never block writers in this design."
	if _, err := parseTranslation(sentencesJSON("Transactions commit in order."), source); err == nil {
		t.Fatal("accepted a reply missing most of the paragraph")
	}
}

func TestAnchorsFallBackProportionallyForHugeParagraphs(t *testing.T) {
	source := strings.Repeat("alpha beta gamma. ", 400)
	model := []string{strings.Repeat("alpha beta gamma. ", 200), strings.Repeat("alpha beta gamma. ", 200)}
	sentences := []TranslationSentence{{Source: model[0]}, {Source: model[1]}}
	anchors := alignAnchors(source+" extra", sentences)
	if compact(anchors[0]+anchors[1]) != compact(source+" extra") {
		t.Fatal("proportional anchors lost text")
	}
}

func TestTranslationPromptAsksForKaTeXInlineMath(t *testing.T) {
	prompt := translationPrompt(Document{Title: "t"}, layoutManifest{Pages: 1}, translationBatch{}, "")
	for _, rule := range []string{"KaTeX", "$T_i$", `\\ll`, `\\$`, "不要输出 $$", "sourceIndex"} {
		if !strings.Contains(prompt, rule) {
			t.Fatalf("prompt lacks %q", rule)
		}
	}
}

func TestTranslationKeepsEscapedKaTeXInTarget(t *testing.T) {
	line := `{"sentences":[{"source":"Let T be the followers of t in MVSG.","target":"令 $T_i$ 为 $t_i$ 在 $MVSG(s, \\ll)$ 中的后继，费用为 \\$5。"}]}`
	sentences, err := parseTranslation(line, "Let T be the followers of t in MVSG.")
	if err != nil {
		t.Fatal(err)
	}
	if want := `令 $T_i$ 为 $t_i$ 在 $MVSG(s, \ll)$ 中的后继，费用为 \$5。`; sentences[0].Target != want {
		t.Fatalf("target %q", sentences[0].Target)
	}
}

func TestTranslationAllowsRepairedCentroidSuperscriptsButRejectsAddedVectorLayers(t *testing.T) {
	for _, pair := range [][2]string{
		{`For each layer ℓ, we compute μ ( jail $ℓ)=\mathbb{E}_{x∈\mathcal{D}_{\mathrm{jail}}}[\mathbf{h}^{(ℓ)}(x)]$.`, `对于每层 $\ell$，质心为 $\mu_{\mathrm{jail}}^{(\ell)}=\mathbb{E}_{x\in\mathcal{D}_{\mathrm{jail}}}[\mathbf{h}^{(\ell)}(x)]$。`},
		{`where μ ( jail $ℓ)∈\mathbb{R}^{d}$ and μ ( ref $ℓ)∈\mathbb{R}^{d}$ denote average representations.`, `其中，$\mu_{\mathrm{jail}}^{(\ell)}\in\mathbb{R}^{d}$ 和 $\mu_{\mathrm{ref}}^{(\ell)}\in\mathbb{R}^{d}$ 为平均表征。`},
		{`The vector is $\mathbf{v}=\bar{\mathbf{h}}^{(l)}$.`, `向量为 $\mathbf{v}=\bar{\mathbf{h}^{(l)}}$。`},
	} {
		if !matchingMathDecorations(pair[0], pair[1]) {
			t.Fatal("valid repair or grouping was rejected", pair)
		}
	}
	if matchingMathDecorations(`The vector is $\mathbf{v}=\bar{\mathbf{h}}^{(l)}$.`, `向量为 $\mathbf{v}^{(l)}=\bar{\mathbf{h}}^{(l)}$。`) {
		t.Fatal("an extra layer was added to v")
	}
}

func TestTranslationUsesSentenceIndicesAndRejectsIncompleteOrConflictingIndices(t *testing.T) {
	source := "First sentence. Second sentence."
	good := `{"sentences":[{"sourceIndex":1,"target":"第二句。"},{"sourceIndex":0,"target":"第一句。另一句中文解释。"}]}`
	parts, err := parseTranslation(good, source)
	if err != nil || len(parts) != 2 || parts[0].Source != "First sentence." || parts[1].Target != "第二句。" {
		t.Fatal(parts, err)
	}
	for _, raw := range []string{
		`{"sentences":[{"sourceIndex":0,"target":"第一句。"}]}`,
		`{"sentences":[{"sourceIndex":0,"target":"第一句。"},{"sourceIndex":0,"target":"重复。"}]}`,
		`{"sentences":[{"sourceIndex":0,"target":"第一句。"},{"sourceIndex":2,"target":"越界。"}]}`,
		`{"sentences":[{"sourceIndex":0,"target":"第一句。"},{"source":"Second sentence.","target":"缺编号。"}]}`,
		`{"sentences":[{"sourceIndex":0,"source":"Wrong source.","target":"第一句。"},{"sourceIndex":1,"target":"第二句。"}]}`,
	} {
		if _, err := parseTranslation(raw, source); err == nil {
			t.Fatal("invalid indexed output accepted", raw)
		}
	}
}
