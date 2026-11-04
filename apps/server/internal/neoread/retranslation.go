package neoread

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Task choices and layout progress are persisted. Connections, capabilities
// and secrets come from the configured provider when the job runs or resumes.
type translationRunOptions struct {
	// One full layout parse per explicit restart; retries resume the same run.
	LayoutRun    string `json:"layoutRun,omitempty"`
	LayoutReady  bool   `json:"layoutReady,omitempty"`
	ClearHistory bool   `json:"clearHistory,omitempty"`
	CleanupDone  bool   `json:"cleanupDone,omitempty"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	Prompt       string `json:"prompt"`
}

func snapshotTranslationOptions(c AIConfig) translationRunOptions {
	model := c.TranslationModels[c.Primary]
	o := translationRunOptions{Provider: c.Primary, Model: model, Effort: c.TranslationEfforts[c.Primary][model]}
	if c.TranslationPrompt != nil {
		o.Prompt = *c.TranslationPrompt
	}
	return o
}

func (o *translationRunOptions) apply(c AIConfig) AIConfig {
	if o == nil || o.Provider == "" {
		return c
	}
	c.Primary = o.Provider
	c.TranslationModels = maps.Clone(c.TranslationModels)
	if c.TranslationModels == nil {
		c.TranslationModels = map[string]string{}
	}
	c.TranslationModels[o.Provider] = o.Model
	c.TranslationEfforts = maps.Clone(c.TranslationEfforts)
	if c.TranslationEfforts == nil {
		c.TranslationEfforts = map[string]map[string]string{}
	}
	c.TranslationEfforts[o.Provider] = maps.Clone(c.TranslationEfforts[o.Provider])
	if c.TranslationEfforts[o.Provider] == nil {
		c.TranslationEfforts[o.Provider] = map[string]string{}
	}
	c.TranslationEfforts[o.Provider][o.Model] = o.Effort
	c.TranslationPrompt = &o.Prompt
	return c
}

func (s *Server) retranslationOptions(p *Processing) (*translationRunOptions, error) {
	if !p.Retranslating {
		return nil, nil
	}
	var body string
	if err := s.Store.DB.QueryRow("SELECT options FROM translation_restarts WHERE document_id=?", p.DocumentID).Scan(&body); err != nil {
		return nil, err
	}
	var options translationRunOptions
	if err := json.Unmarshal([]byte(body), &options); err != nil {
		return nil, err
	}
	return &options, nil
}

func (s *Server) translationPrevious(documentID string) (map[string]bool, error) {
	rows, err := s.Store.DB.Query("SELECT block_id,source_hash FROM translation_previous WHERE document_id=?", documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	previous := map[string]bool{}
	for rows.Next() {
		var block, hash string
		if err := rows.Scan(&block, &hash); err != nil {
			return nil, err
		}
		previous[block+":"+hash] = true
	}
	return previous, rows.Err()
}

func (s *Server) requestRetranslation(w http.ResponseWriter, r *http.Request, restart ...bool) {
	fullRestart := len(restart) > 0 && restart[0]
	fresh := len(restart) > 1 && restart[1]
	fullRestart = fullRestart || fresh
	lock := s.annotationRecoveryLock(r.PathValue("id"))
	if !lock.TryLock() {
		fail(w, 409, "正在检查批注，请稍后重新翻译")
		return
	}
	defer lock.Unlock()
	config := s.aiConfig()
	if !fresh && (!validAgent(config.Primary) || !(primaryCapable(config, taskTranslation) || capable(config, "text-api", false))) {
		fail(w, 409, "请先配置并检测翻译模型")
		return
	}
	s.processingControlMu.Lock()
	defer s.processingControlMu.Unlock()
	d, err := s.Store.Document(r.PathValue("id"))
	if err != nil || d.DeletedAt != "" {
		fail(w, 404, "文档不存在")
		return
	}
	if !fresh && (d.Type != "pdf" || d.Library == "books") {
		fail(w, 400, "全文重译仅适用于论文；图书请按章节翻译")
		return
	}
	m, err := s.readTranslationSource(d.ID)
	if err != nil && !fresh {
		fail(w, 409, "正文仍在解析中")
		return
	}
	s.processingMu.Lock()
	p, err := s.Store.processing(d.ID)
	if fresh && errors.Is(err, sql.ErrNoRows) {
		p = initialDocumentProcessing(d, "")
		err = s.Store.saveProcessing(p)
	}
	if err != nil || !fullRestart && p.Status != "complete" {
		s.processingMu.Unlock()
		fail(w, 409, "请等待当前翻译完成后再重新翻译")
		return
	}
	if s.blockedProcessing == nil {
		s.blockedProcessing = map[string]bool{}
	}
	s.blockedProcessing[d.ID] = true
	task := s.activeProcessing[d.ID]
	if task != nil {
		task.cancel()
	}
	s.processingMu.Unlock()
	if task != nil {
		<-task.done
	}
	if fresh {
		finish, e := s.beginDocumentReparse(d)
		if e != nil {
			s.processingMu.Lock()
			delete(s.blockedProcessing, d.ID)
			s.processingMu.Unlock()
			fail(w, 409, e.Error())
			return
		}
		defer finish()
	}
	s.processingMu.Lock()
	defer s.processingMu.Unlock()
	defer delete(s.blockedProcessing, d.ID)
	// Joining prevents a previous worker's delayed results from replacing this run.
	p, err = s.Store.processing(d.ID)
	if err == nil {
		m, err = s.readTranslationSource(d.ID)
	}
	if fresh && p.DocumentID != "" {
		err = s.queueFreshRetranslation(p, snapshotTranslationOptions(config))
	} else if err == nil {
		err = s.queueRetranslation(p, m, snapshotTranslationOptions(config), fullRestart)
	}
	if err != nil {
		fail(w, 409, "无法开始重新翻译："+err.Error())
		return
	}
	p, err = s.Store.processing(d.ID)
	if err != nil {
		fail(w, 500, "无法读取翻译任务")
		return
	}
	respond(w, 200, p)
}

func (s *Server) queueFreshRetranslation(p Processing, options translationRunOptions) error {
	options.LayoutRun, options.LayoutReady, options.ClearHistory, options.CleanupDone = id(), false, true, false
	s.translationMu.Lock()
	s.Store.processingWriteMu.Lock()
	tx, err := s.Store.DB.Begin()
	if err != nil {
		s.Store.processingWriteMu.Unlock()
		s.translationMu.Unlock()
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT body FROM translations WHERE document_id=? UNION ALL SELECT body FROM translation_previous WHERE document_id=?", p.DocumentID, p.DocumentID)
	var previous []TranslationBlock
	if err == nil {
		for rows.Next() {
			var body string
			var item TranslationBlock
			if err = rows.Scan(&body); err != nil {
				break
			}
			if err = json.Unmarshal([]byte(body), &item); err != nil {
				break
			}
			if item.Status == "complete" {
				previous = append(previous, item)
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
	}
	if err == nil {
		err = captureAnnotationAnchors(tx, p.DocumentID, previous)
	}
	for _, table := range []string{"translations", "translation_previous", "translation_restarts", "document_corpora", "document_corpus_jobs", "search_index", "document_passages_fts", "book_layout_jobs"} {
		if err != nil {
			break
		}
		_, err = tx.Exec("DELETE FROM "+table+" WHERE document_id=?", p.DocumentID)
	}
	p.Enabled, p.Retranslating, p.ManualTranslation = true, true, false
	p.AutoTranslateMaxPages = nil
	p.Chapter = nil
	p.LayoutRevision = ""
	p.CompletedAt = ""
	p.Warning = ""
	p.PagesDone = 0
	p.TranslationsDone = 0
	p.TranslationsTotal = 0
	p.Translating = &ProcessingStage{Status: "queued", Detail: "等待清理并重新解析翻译"}
	aggregateProcessing(&p)
	if err == nil {
		_, err = tx.Exec("INSERT INTO translation_restarts(document_id,options) VALUES(?,?)", p.DocumentID, jsonText(options))
	}
	if err == nil {
		_, err = tx.Exec("INSERT INTO document_processing(document_id,phase,status,body) VALUES(?,?,?,?) ON CONFLICT(document_id) DO UPDATE SET phase=excluded.phase,status=excluded.status,body=excluded.body", p.DocumentID, p.Phase, p.Status, jsonText(p))
	}
	if err == nil {
		err = tx.Commit()
	}
	s.Store.processingWriteMu.Unlock()
	s.translationMu.Unlock()
	if err != nil {
		return err
	}
	if err = s.clearDocumentParseFiles(p.DocumentID); err != nil {
		// The reset is already committed. Keep the accepted job recoverable;
		// its worker retries cleanup before parsing and reports any failure.
		s.diagnosticEvent(slog.LevelWarn, "document.reset.cleanup_deferred", "documentId", p.DocumentID, "errorCode", errorKind(err))
		return nil
	}
	options.CleanupDone = true
	_, err = s.Store.DB.Exec("UPDATE translation_restarts SET options=? WHERE document_id=?", jsonText(options), p.DocumentID)
	return err
}

func (s *Server) clearDocumentParseFiles(documentID string) error {
	if filepath.Base(documentID) != documentID || documentID == "." || documentID == ".." {
		return errors.New("文档编号无效")
	}
	root := filepath.Join(s.Store.Root, "cache")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == documentID || strings.HasPrefix(name, documentID+"-reparse-") || strings.HasPrefix(name, documentID+"-previous-") {
			if err = os.RemoveAll(filepath.Join(root, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) queueRetranslation(p Processing, m translationInput, options translationRunOptions, allowEmpty ...bool) error {
	options.LayoutRun, options.LayoutReady = id(), false
	items, err := s.translations(p.DocumentID, m)
	if err != nil {
		return err
	}
	previousItems := slices.Clone(items)
	text := map[string]bool{}
	for _, paragraph := range m.translationParagraphs() {
		text[paragraph.BlockID+":"+paragraph.SourceHash] = true
	}
	count := 0
	for i := range items {
		if text[items[i].BlockID+":"+items[i].SourceHash] && items[i].Status == "complete" {
			items[i].Status = "pending"
			items[i].Sentences = []TranslationSentence{}
			items[i].Error = ""
			count++
		}
	}
	if count == 0 && !(len(allowEmpty) > 0 && allowEmpty[0]) {
		return errors.New("没有已完成的正文译文")
	}
	s.translationMu.Lock()
	defer s.translationMu.Unlock()
	s.Store.processingWriteMu.Lock()
	defer s.Store.processingWriteMu.Unlock()
	tx, err := s.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = captureAnnotationAnchors(tx, p.DocumentID, previousItems); err != nil {
		return err
	}
	for _, item := range items {
		if !text[item.BlockID+":"+item.SourceHash] || item.Status != "pending" {
			continue
		}
		if _, err = tx.Exec(`INSERT INTO translation_previous(document_id,block_id,source_hash,body)
SELECT document_id,block_id,source_hash,body FROM translations WHERE document_id=? AND block_id=? AND source_hash=? AND status='complete'
ON CONFLICT(document_id,block_id,source_hash) DO UPDATE SET body=excluded.body`, p.DocumentID, item.BlockID, item.SourceHash); err != nil {
			return err
		}
		body, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE translations SET status='pending',body=? WHERE document_id=? AND block_id=? AND source_hash=?", string(body), p.DocumentID, item.BlockID, item.SourceHash); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("INSERT INTO translation_restarts(document_id,options) VALUES(?,?) ON CONFLICT(document_id) DO UPDATE SET options=excluded.options", p.DocumentID, jsonText(options)); err != nil {
		return err
	}
	p.Enabled, p.Retranslating = true, true
	p.AutoTranslateMaxPages, p.ManualTranslation = nil, false
	p.Phase, p.Status, p.Detail = "translating", "queued", "等待重新解析版面"
	p.CompletedAt, p.Warning = "", ""
	p.TranslationsDone, p.TranslationsTotal = translationCounts(items)
	p.Translating = &ProcessingStage{Status: "queued", Detail: "等待重新解析版面"}
	p.UpdatedAt = now()
	if _, err = tx.Exec("UPDATE document_processing SET phase=?,status=?,body=? WHERE document_id=?", p.Phase, p.Status, jsonText(p), p.DocumentID); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO annotation_recovery(document_id,checked) VALUES(?,0) ON CONFLICT(document_id) DO UPDATE SET checked=0", p.DocumentID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) retryCachedTranslation(documentID string, previous TranslationBlock) error {
	s.translationMu.Lock()
	defer s.translationMu.Unlock()
	tx, err := s.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = captureAnnotationAnchors(tx, documentID, []TranslationBlock{previous}); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO translation_previous(document_id,block_id,source_hash,body)
SELECT document_id,block_id,source_hash,body FROM translations WHERE document_id=? AND block_id=? AND source_hash=? AND status='complete'
ON CONFLICT(document_id,block_id,source_hash) DO UPDATE SET body=excluded.body`, documentID, previous.BlockID, previous.SourceHash); err != nil {
		return err
	}
	pending := TranslationBlock{BlockID: previous.BlockID, SourceHash: previous.SourceHash, Status: "pending", Sentences: []TranslationSentence{}}
	body, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE translations SET status='pending',body=? WHERE document_id=? AND block_id=? AND source_hash=?", string(body), documentID, previous.BlockID, previous.SourceHash); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.publishTranslation(documentID, previous)
	return nil
}
