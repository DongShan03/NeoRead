package neoread

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

func (s *Store) deleteChatSession(ctx context.Context, document, session string) ([]string, map[string]bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	messages := chatTable(document, "messages")
	conversations := chatTable(document, "document_conversations")
	ids := []string{}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM "+messages+" WHERE document_id=? AND json_extract(body,'$.sessionId')=?", document, session)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	prefix := "session:" + session + ":"
	states := map[string]bool{}
	rows, err = tx.QueryContext(ctx, "SELECT body FROM "+conversations+" WHERE document_id=? AND substr(provider,1,?)=?", document, len(prefix), prefix)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var raw string
		var state documentConversation
		if err = rows.Scan(&raw); err == nil {
			err = json.Unmarshal([]byte(raw), &state)
		}
		if err != nil {
			break
		}
		states[state.ID] = true
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	tail := "SELECT id FROM " + messages + " WHERE document_id=? AND json_extract(body,'$.sessionId')=?"
	if document != libraryChatOwner {
		if _, err = tx.ExecContext(ctx, "UPDATE annotations SET body=json_set(body,'$.answerId','','$.resolved',json('false')) WHERE document_id=? AND json_extract(body,'$.answerId') IN ("+tail+")", document, document, session); err != nil {
			return nil, nil, err
		}
	}
	statements := []struct {
		query string
		args  []any
	}{
		{"DELETE FROM " + chatTable(document, "chat_agent_runs") + " WHERE document_id=? AND json_extract(body,'$.messageId') IN (" + tail + ")", []any{document, document, session}},
		{"DELETE FROM " + chatTable(document, "chat_runs") + " WHERE session_id=?", []any{session}},
		{"DELETE FROM " + conversations + " WHERE document_id=? AND substr(provider,1,?)=?", []any{document, len(prefix), prefix}},
		{"DELETE FROM " + messages + " WHERE document_id=? AND json_extract(body,'$.sessionId')=?", []any{document, session}},
		{"DELETE FROM " + chatTable(document, "chat_sessions") + " WHERE document_id=? AND id=?", []any{document, session}},
	}
	for _, stmt := range statements {
		if _, err = tx.ExecContext(ctx, stmt.query, stmt.args...); err != nil {
			return nil, nil, err
		}
	}
	return ids, states, tx.Commit()
}

func (s *Server) deleteChatSession(w http.ResponseWriter, r *http.Request) {
	document, session := r.PathValue("id"), r.PathValue("session")
	if document != libraryChatOwner {
		doc, err := s.Store.Document(document)
		if err != nil || doc.DeletedAt != "" {
			fail(w, 404, "文档不存在")
			return
		}
	}
	ctx, finish, err := s.beginChatTask(r.Context(), document, session)
	if err != nil {
		fail(w, 409, "书库或文档正在关闭")
		return
	}
	defer finish()
	s.annotationMu.Lock()
	defer s.annotationMu.Unlock()
	s.chatMu.Lock()
	if _, err = s.Store.chatSession(document, session); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.chatMu.Unlock()
			fail(w, 500, "无法读取会话，请重试")
			return
		}
		var owner string
		missing := s.Store.DB.QueryRowContext(ctx, "SELECT document_id FROM "+chatTable(document, "chat_sessions")+" WHERE id=?", session).Scan(&owner)
		s.chatMu.Unlock()
		if errors.Is(err, sql.ErrNoRows) && errors.Is(missing, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		fail(w, 404, "会话不存在")
		return
	}
	if s.chatRuns[session] != nil || s.chatReservations[session] {
		s.chatMu.Unlock()
		fail(w, 409, "请先停止此会话的回答，再删除会话")
		return
	}
	ids, states, err := s.Store.deleteChatSession(ctx, document, session)
	s.chatMu.Unlock()
	if err != nil {
		s.diagnosticEvent(slog.LevelError, "chat.session_delete_failed", "requestId", requestID(r.Context()), "documentId", document, "sessionId", session, "errorCode", "save")
		fail(w, 500, "无法删除会话，原记录已保留，请重试")
		return
	}
	s.clearChatAnswerHistory(document, ids)
	s.documentCodex.deleteConversations(states)
	s.codexChat.deleteMessages(document, ids)
	if document != libraryChatOwner {
		s.publishAnnotationChange(document)
	}
	s.diagnosticEvent(slog.LevelInfo, "chat.session_deleted", "requestId", requestID(r.Context()), "documentId", document, "sessionId", session)
	w.WriteHeader(http.StatusNoContent)
}
