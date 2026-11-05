package neoread

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

func (s *Server) hideCodexHistory() bool {
	var value string
	if s.Store.DB.QueryRow("SELECT value FROM settings WHERE key='neoread'").Scan(&value) != nil {
		return false
	}
	var settings struct {
		Hide bool `json:"hideCodexHistory"`
	}
	return json.Unmarshal([]byte(value), &settings) == nil && settings.Hide
}

func (s *Server) trackCodexThread(ctx context.Context, document, thread string) error {
	if thread == "" {
		return nil
	}
	_, err := s.Store.DB.ExecContext(ctx, "INSERT OR IGNORE INTO codex_history_threads(thread_id,document_id,hidden) VALUES(?,?,0)", thread, document)
	return err
}

// Archiving unloads the native thread. Always unarchive before resuming it;
// keep its ID and committed turns so continuation never starts from scratch.
func (s *Server) resumeCodexHistory(ctx context.Context, session *codexSession, thread string) error {
	var hidden bool
	err := s.Store.DB.QueryRowContext(ctx, "SELECT hidden FROM codex_history_threads WHERE thread_id=?", thread).Scan(&hidden)
	tracked := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if hidden {
		_, _ = session.control(ctx, "thread/unarchive", map[string]string{"threadId": thread})
	}
	err = session.resumeDocument(ctx, thread)
	if err != nil && tracked && !hidden && errorKind(err) != ErrorBusy {
		// A lost archive acknowledgement can leave the local marker behind.
		if _, restoreErr := session.control(ctx, "thread/unarchive", map[string]string{"threadId": thread}); restoreErr == nil {
			err = session.resumeDocument(ctx, thread)
		}
	}
	if err == nil && tracked {
		_, err = s.Store.DB.ExecContext(ctx, "UPDATE codex_history_threads SET hidden=0 WHERE thread_id=?", thread)
	}
	return err
}

func (s *Server) archiveCodexAfterTurn(session *codexSession, document string) bool {
	if session.threadID == "" {
		return false
	}
	s.codexHistoryMu.Lock()
	defer s.codexHistoryMu.Unlock()
	if !s.hideCodexHistory() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.trackCodexThread(ctx, document, session.threadID)
	if err == nil {
		_, err = session.control(ctx, "thread/archive", map[string]string{"threadId": session.threadID})
	}
	if err == nil {
		_, err = s.Store.DB.ExecContext(ctx, "UPDATE codex_history_threads SET hidden=1 WHERE thread_id=?", session.threadID)
	}
	if err != nil {
		s.diagnosticEvent(slog.LevelError, "codex.history.archive_failed", "errorCode", string(errorKind(err)))
	}
	// Even a lost response may have unloaded the thread. Reopen through the
	// normal resume path before the next question instead of reusing this cache.
	return true
}

// Only IDs recorded by NeoRead are touched. Active document runs finish first
// and apply the current preference in archiveCodexAfterTurn.
func (s *Server) applyCodexHistoryVisibility(ctx context.Context) error {
	s.codexHistoryMu.Lock()
	defer s.codexHistoryMu.Unlock()
	s.documentMu.Lock()
	closed := s.documentClosed
	s.documentMu.Unlock()
	if closed {
		return context.Canceled
	}
	hide := s.hideCodexHistory()
	if _, err := s.Store.DB.ExecContext(ctx, `INSERT OR IGNORE INTO codex_history_threads(thread_id,document_id,hidden)
 SELECT json_extract(body,'$.nativeThread'),document_id,0 FROM document_conversations
 WHERE COALESCE(json_extract(body,'$.nativeThread'),'')<>'' AND (provider='codex' OR substr(provider,-6)=':codex')
 UNION ALL SELECT json_extract(body,'$.nativeThread'),document_id,0 FROM library_document_conversations
 WHERE COALESCE(json_extract(body,'$.nativeThread'),'')<>'' AND (provider='codex' OR substr(provider,-6)=':codex')`); err != nil {
		return err
	}
	rows, err := s.Store.DB.QueryContext(ctx, "SELECT thread_id,document_id,hidden FROM codex_history_threads")
	if err != nil {
		return err
	}
	type entry struct {
		thread, document string
		hidden           bool
	}
	entries := []entry{}
	for rows.Next() {
		var item entry
		if err = rows.Scan(&item.thread, &item.document, &item.hidden); err != nil {
			break
		}
		if item.hidden != hide {
			entries = append(entries, item)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil || len(entries) == 0 {
		return err
	}
	s.documentCodex.closeIdleForHistory()
	work := filepath.Join(s.Store.Root, "ai-work", "codex-history-control")
	if err = os.MkdirAll(work, 0700); err != nil {
		return err
	}
	session, err := newCodexSession(work, "")
	if err != nil {
		return err
	}
	defer session.close()
	if err = session.initializeControl(ctx); err != nil {
		return err
	}
	method := "thread/unarchive"
	if hide {
		method = "thread/archive"
	}
	failed, changed := 0, 0
	for _, item := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.documentMu.Lock()
		if s.documentClosed || len(s.documentTasks[item.document]) > 0 {
			s.documentMu.Unlock()
			continue
		}
		_, err = session.control(ctx, method, map[string]string{"threadId": item.thread})
		if err == nil {
			_, err = s.Store.DB.ExecContext(ctx, "UPDATE codex_history_threads SET hidden=? WHERE thread_id=?", hide, item.thread)
		}
		s.documentMu.Unlock()
		if err != nil {
			failed++
		} else {
			changed++
		}
	}
	level := slog.LevelInfo
	if failed > 0 {
		level = slog.LevelError
	}
	s.diagnosticEvent(level, "codex.history.visibility_applied", "hidden", hide, "changed", changed, "failed", failed)
	if failed > 0 {
		return errors.New("设置已保存，部分 Codex 对话尚未完成归档或恢复，聊天记录已保留")
	}
	return nil
}

func (s *Server) StartCodexHistory(ctx context.Context) {
	go func() {
		work, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := s.applyAgentHistoryVisibility(work); err != nil && !errors.Is(err, context.Canceled) {
			s.diagnosticEvent(slog.LevelError, "agent.history.visibility_failed", "errorCode", string(errorKind(err)))
		}
	}()
}
