package neoread

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const libraryChatSchema = `
 CREATE TABLE IF NOT EXISTS library_chat_sessions(id TEXT PRIMARY KEY,document_id TEXT NOT NULL DEFAULT 'library',title TEXT NOT NULL,provider TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,scope TEXT NOT NULL,title_manual INTEGER NOT NULL DEFAULT 0 CHECK(title_manual IN (0,1)));
 CREATE TABLE IF NOT EXISTS library_messages(id TEXT PRIMARY KEY,document_id TEXT NOT NULL,body TEXT NOT NULL,created_at TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS library_messages_session ON library_messages(json_extract(body,'$.sessionId'));
 CREATE TABLE IF NOT EXISTS library_chat_runs(id TEXT PRIMARY KEY,session_id TEXT NOT NULL REFERENCES library_chat_sessions(id) ON DELETE CASCADE,body TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS library_chat_runs_session ON library_chat_runs(session_id);
 CREATE TABLE IF NOT EXISTS library_document_conversations(document_id TEXT NOT NULL,provider TEXT NOT NULL,body TEXT NOT NULL,PRIMARY KEY(document_id,provider));
 CREATE TABLE IF NOT EXISTS library_chat_agent_runs(id TEXT PRIMARY KEY,document_id TEXT NOT NULL,body TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS library_processing_usage(id TEXT PRIMARY KEY,document_id TEXT NOT NULL,body TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS library_chat_usage_coverage(document_id TEXT PRIMARY KEY,history_complete INTEGER NOT NULL);
`

const libraryChatOwner = "library"

func chatTable(owner, table string) string {
	if owner == libraryChatOwner {
		return "library_" + table
	}
	return table
}
func chatRunTable(session string) string {
	if strings.HasPrefix(session, "library-") {
		return "library_chat_runs"
	}
	return "chat_runs"
}

type libraryChatScope struct {
	Category    string   `json:"category"`
	CategoryKey string   `json:"categoryKey,omitempty"`
	DocumentIDs []string `json:"documentIds"`
}
type libraryChatSession struct {
	ChatSession
	Scope libraryChatScope `json:"scope"`
}

func (s *Store) libraryScope(session string) (libraryChatScope, error) {
	var raw string
	var scope libraryChatScope
	err := s.DB.QueryRow("SELECT scope FROM library_chat_sessions WHERE id=?", session).Scan(&raw)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &scope)
	}
	return scope, err
}

func (s *Server) beginChatTask(parent context.Context, owner, session string) (context.Context, func(), error) {
	if owner != libraryChatOwner {
		return s.beginDocumentTask(parent, owner)
	}
	s.documentMu.Lock()
	defer s.documentMu.Unlock()
	if s.documentClosed {
		return nil, nil, errors.New("书库正在关闭")
	}
	ctx, finish := s.registerDocumentTask(parent, "library-chat/"+session)
	return ctx, finish, nil
}

func (s *Server) registerLibraryChat(mux *http.ServeMux) {
	wrap := func(handler http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("id", libraryChatOwner)
			handler(w, r)
		}
	}
	mux.HandleFunc("GET /api/library/chat-sessions", s.listLibraryChatSessions)
	mux.HandleFunc("POST /api/library/chat-sessions", s.createLibraryChatSession)
	mux.HandleFunc("PATCH /api/library/chat-sessions/{session}", s.updateLibraryChatSession)
	mux.HandleFunc("DELETE /api/library/chat-sessions/{session}", wrap(s.deleteChatSession))
	mux.HandleFunc("GET /api/library/chat-sessions/{session}/messages", wrap(s.sessionMessages))
	mux.HandleFunc("POST /api/library/chat-sessions/{session}/chat", s.startLibraryChat)
	mux.HandleFunc("GET /api/library/chat-sessions/{session}/runs/{run}/events", wrap(s.sessionRunEvents))
	mux.HandleFunc("POST /api/library/chat-sessions/{session}/runs/{run}/cancel", wrap(s.cancelSessionRun))
	mux.HandleFunc("PATCH /api/library/chat-sessions/{session}/messages/{message}", s.changeLibraryChatMessage)
	mux.HandleFunc("DELETE /api/library/chat-sessions/{session}/messages/{message}", s.changeLibraryChatMessage)
	mux.HandleFunc("GET /api/library/chat-sessions/{session}/messages/{message}/sources/{source}", s.validateLibraryChatEvidence)
}

func (s *Server) listLibraryChatSessions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.DB.QueryContext(r.Context(), `SELECT s.id,s.document_id,s.title,s.provider,s.created_at,s.updated_at,s.scope,s.title_manual,
    (SELECT json_remove(r.body,'$.events') FROM library_chat_runs r WHERE r.session_id=s.id ORDER BY r.rowid DESC LIMIT 1)
    FROM library_chat_sessions s ORDER BY s.rowid DESC`)
	if err != nil {
		fail(w, 500, "无法读取书库对话")
		return
	}
	items := []libraryChatSession{}
	for rows.Next() {
		var item libraryChatSession
		var scope string
		var run sql.NullString
		if err = rows.Scan(&item.ID, &item.DocumentID, &item.Title, &item.Provider, &item.CreatedAt, &item.UpdatedAt, &scope, &item.TitleManual, &run); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(scope), &item.Scope); err != nil {
			break
		}
		if run.Valid {
			item.LatestRun = &ChatRun{}
			if err = json.Unmarshal([]byte(run.String), item.LatestRun); err != nil {
				break
			}
		}
		items = append(items, item)
	}
	err = errors.Join(err, rows.Err())
	rows.Close()
	if err != nil {
		fail(w, 500, "无法读取书库对话")
		return
	}
	respond(w, 200, items)
}

func (s *Server) createLibraryChatSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID          string           `json:"id"`
		Title       string           `json:"title"`
		TitleManual bool             `json:"titleManual"`
		Provider    string           `json:"provider"`
		Scope       libraryChatScope `json:"scope"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Title == "" {
		req.Title = "新会话"
	}
	title, titleErr := metadataText(json.RawMessage(jsonText(req.Title)), 80, false)
	if titleErr != nil || req.ID != "" && (!strings.HasPrefix(req.ID, "library-") || !syncID(strings.TrimPrefix(req.ID, "library-"))) {
		fail(w, 400, "会话名称或标识无效")
		return
	}
	if !validAgent(req.Provider) || strings.TrimSpace(req.Scope.Category) == "" || utf8.RuneCountInString(req.Scope.Category) > 256 || utf8.RuneCountInString(req.Scope.CategoryKey) > 1024 || strings.ContainsFunc(req.Scope.CategoryKey, unicode.IsControl) || len(req.Scope.DocumentIDs) == 0 {
		fail(w, 400, "请选择分类中的论文及可用 Agent")
		return
	}
	req.Scope.DocumentIDs = slices.Compact(slices.Sorted(slices.Values(req.Scope.DocumentIDs)))
	for _, id := range req.Scope.DocumentIDs {
		doc, err := s.Store.Document(id)
		if err != nil || doc.DeletedAt != "" || doc.Library != "papers" {
			fail(w, 409, "所选论文已移除，请刷新书库后重试")
			return
		}
	}
	if req.ID == "" {
		req.ID = "library-" + id()
	}
	item := libraryChatSession{ChatSession: ChatSession{ID: req.ID, DocumentID: libraryChatOwner, Title: title, TitleManual: req.TitleManual, Provider: req.Provider, CreatedAt: now(), UpdatedAt: now()}, Scope: req.Scope}
	if _, err := s.Store.DB.ExecContext(r.Context(), "INSERT INTO library_chat_sessions(id,document_id,title,provider,created_at,updated_at,scope,title_manual) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING", item.ID, item.DocumentID, item.Title, item.Provider, item.CreatedAt, item.UpdatedAt, jsonText(item.Scope), item.TitleManual); err != nil {
		fail(w, 500, "无法创建书库对话")
		return
	}
	existing, e := s.Store.libraryScope(item.ID)
	if e != nil || syncHash(existing) != syncHash(item.Scope) {
		fail(w, 409, "会话标识已被其他分类使用")
		return
	}
	saved, e := s.Store.chatSession(libraryChatOwner, item.ID)
	if e != nil {
		fail(w, 500, "无法读取书库会话")
		return
	}
	item.ChatSession = saved
	respond(w, 201, item)
}

func (s *Server) updateLibraryChatSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title    *string `json:"title"`
		Provider *string `json:"provider"`
	}
	if !decode(w, r, &req) {
		return
	}
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	item, err := s.Store.chatSession(libraryChatOwner, r.PathValue("session"))
	if err != nil {
		fail(w, 404, "会话不存在")
		return
	}
	if req.Title != nil {
		title, e := metadataText(json.RawMessage(jsonText(*req.Title)), 80, false)
		if e != nil {
			fail(w, 400, "会话名称无效")
			return
		}
		item.Title = title
		item.TitleManual = true
	}
	if req.Provider != nil {
		if !validAgent(*req.Provider) {
			fail(w, 400, "Agent 无效")
			return
		}
		if s.chatRuns[item.ID] != nil || s.chatReservations[item.ID] {
			fail(w, 409, "请等待当前回答完成")
			return
		}
		item.Provider = *req.Provider
	}
	item.UpdatedAt = now()
	if _, err = s.Store.DB.Exec("UPDATE library_chat_sessions SET title=?,title_manual=?,provider=?,updated_at=? WHERE id=?", item.Title, item.TitleManual, item.Provider, item.UpdatedAt, item.ID); err != nil {
		fail(w, 500, "无法保存会话")
		return
	}
	respond(w, 200, item)
}

func (s *Server) startLibraryChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider      string          `json:"provider"`
		Prompt        string          `json:"prompt"`
		Context       string          `json:"context"`
		UserMessageID string          `json:"userMessageId"`
		WebSearch     *bool           `json:"webSearch"`
		References    json.RawMessage `json:"references"`
		Attachments   []string        `json:"attachments"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !validAgent(req.Provider) || strings.TrimSpace(req.Prompt) == "" || len(req.Prompt) > 16000 || len(req.Context) > chatContextBytes || req.UserMessageID != "" && !chatMessageIDPattern.MatchString(req.UserMessageID) || len(req.Attachments) > 0 {
		fail(w, 400, "问题或引用长度无效")
		return
	}
	scope, err := s.Store.libraryScope(r.PathValue("session"))
	if err != nil {
		fail(w, 404, "书库会话不存在")
		return
	}
	r.SetPathValue("id", libraryChatOwner)
	var search []bool
	if req.WebSearch != nil {
		search = []bool{*req.WebSearch}
	}
	s.chatDocument(w, r, Document{ID: libraryChatOwner, Title: scope.Category}, req.Provider, req.Prompt, req.Context, req.References, nil, nil, req.UserMessageID, search...)
}

func (s *Server) changeLibraryChatMessage(w http.ResponseWriter, r *http.Request) {
	r.SetPathValue("id", libraryChatOwner)
	s.changeSessionMessage(w, r)
}
