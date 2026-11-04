package neoread

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Controls serialize with worker claims. Cancellation joins processing only,
// leaving independent chat requests and already published results intact.
func (s *Server) getAssistance(w http.ResponseWriter, r *http.Request) {
	d, e := s.Store.Document(r.PathValue("id"))
	if e != nil || (d.Type != "pdf" && d.Type != "epub") {
		fail(w, 404, "文档不存在")
		return
	}
	p, e := s.Store.processing(d.ID)
	if e != nil {
		p = initialProcessing(d.ID)
		if d.Type == "epub" {
			p = initialDocumentProcessing(d, "")
		}
	}
	respond(w, 200, p)
}
func (s *Server) setAssistance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action       string `json:"action"`
		Restart      bool   `json:"restart,omitempty"`
		ClearHistory bool   `json:"clearHistory,omitempty"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Action == "retranslate" {
		s.requestRetranslation(w, r, req.Restart, req.ClearHistory)
		return
	}
	if req.Action == "prioritize" {
		s.prioritizeTranslation(w, r)
		return
	}
	if req.Action != "start" && req.Action != "pause" && req.Action != "resume" {
		fail(w, 400, "无效的辅助阅读操作")
		return
	}
	s.processingControlMu.Lock()
	defer s.processingControlMu.Unlock()
	d, e := s.Store.Document(r.PathValue("id"))
	if e != nil || (d.Type != "pdf" && d.Type != "epub") {
		fail(w, 404, "文档不存在")
		return
	}
	s.processingMu.Lock()
	p, e := s.Store.processing(d.ID)
	if e != nil {
		p = initialProcessing(d.ID)
		if d.Type == "epub" {
			p = initialDocumentProcessing(d, "")
		}
	}
	// Initial paper parsing is mandatory. Translation controls must neither
	// cancel its worker nor discard the import-time translation preference.
	if d.Type == "pdf" && d.Library == "papers" && p.Phase == "learning" && !p.Retranslating {
		defer s.processingMu.Unlock()
		if req.Action == "pause" {
			fail(w, 409, "论文解析不能暂停；解析完成后可暂停翻译")
			return
		}
		if !p.Enabled || (p.Status != "queued" && p.Status != "running") {
			if !p.Enabled {
				limit := 0
				p.AutoTranslateMaxPages = &limit
			}
			p.Enabled, p.Status, p.Detail = true, "queued", "等待解析 PDF"
			p.CompletedAt = ""
			if e = s.Store.saveProcessing(p); e != nil {
				fail(w, 500, "无法恢复解析任务")
				return
			}
			if p, e = s.Store.processing(d.ID); e != nil {
				fail(w, 500, "无法读取处理状态")
				return
			}
		}
		respond(w, 200, p)
		return
	}
	if s.blockedProcessing == nil {
		s.blockedProcessing = map[string]bool{}
	}
	s.blockedProcessing[d.ID] = true
	// Disable before joining, so neither serial queue can claim a new stage.
	p.Enabled = false
	p.Status = "paused"
	if e = s.Store.saveProcessing(p); e != nil {
		delete(s.blockedProcessing, d.ID)
		s.processingMu.Unlock()
		fail(w, 500, "无法暂停处理")
		return
	}
	task := s.activeProcessing[d.ID]
	if task != nil {
		task.cancel()
	}
	s.processingMu.Unlock()
	if task != nil {
		<-task.done
	}
	s.processingMu.Lock()
	defer s.processingMu.Unlock()
	defer delete(s.blockedProcessing, d.ID)
	// Worker may have published final progress while cancellation was joining.
	if latest, err := s.Store.processing(d.ID); err == nil {
		p = latest
	} else {
		fail(w, 404, "文档不存在")
		return
	}
	p.Enabled = req.Action != "pause"
	p.CompletedAt = ""
	if p.Enabled {
		p.AutoTranslateMaxPages = nil
		p.ManualTranslation = false
		p.Status, p.Detail = "queued", "等待继续处理"
		// Prepare the complete manifest; cached completed results remain reusable.
		// Completed translations and formula results are reused.
		if d.Type == "epub" || p.Retranslating {
			p.Phase = "translating"
			p.Translating = &ProcessingStage{Status: "queued", Detail: "等待继续翻译"}
		} else {
			p.Phase = "learning"
			p.PagesDone = 0
			p.Translating = nil
		}
		if e = s.retryFailedTranslations(d.ID); e != nil {
			fail(w, 500, "无法恢复译文")
			return
		}
	} else {
		p.Status, p.Detail = "paused", "翻译已暂停"
	}

	if e = s.Store.saveProcessing(p); e != nil {
		fail(w, 500, "无法保存辅助阅读设置")
		return
	}
	p, _ = s.Store.processing(d.ID)
	respond(w, 200, p)
}

func (s *Server) prioritizeTranslation(w http.ResponseWriter, r *http.Request) {
	s.processingControlMu.Lock()
	defer s.processingControlMu.Unlock()
	s.processingMu.Lock()
	defer s.processingMu.Unlock()
	d, err := s.Store.Document(r.PathValue("id"))
	if err != nil || d.DeletedAt != "" {
		fail(w, 404, "文档不存在")
		return
	}
	p, err := s.Store.processing(d.ID)
	if err != nil || !p.Enabled || p.Phase != "translating" || (p.Status != "queued" && p.Status != "running") {
		fail(w, 409, "此文档没有正在进行的翻译任务")
		return
	}
	if _, err := s.Store.DB.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", "translation-priority:"+d.ID, strconv.FormatInt(time.Now().UnixNano(), 10)); err != nil {
		fail(w, 500, "无法保存翻译优先级")
		return
	}
	// Reuse normal cancellation: completed paragraphs survive, and unfinished
	// requests requeue with their previous translations intact.
	for id, task := range s.activeProcessing {
		if id == d.ID {
			continue
		}
		if current, err := s.Store.processing(id); err == nil && current.Phase == "translating" {
			task.cancel()
		}
	}
	s.diagnosticEvent(slog.LevelInfo, "translation.prioritized", "documentId", d.ID)
	respond(w, 200, p)
}
func (s *Server) finishProcessingTask(id string, t *documentTask) {
	if t == nil {
		return
	}
	t.cancel()
	s.processingMu.Lock()
	defer s.processingMu.Unlock()
	delete(s.activeProcessing, id)
	close(t.done)
}
