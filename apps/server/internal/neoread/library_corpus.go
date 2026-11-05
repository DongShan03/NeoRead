package neoread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
)

type libraryCorpusPaper struct {
	ID        string `json:"documentId"`
	Title     string `json:"title"`
	Available bool   `json:"available"`
	Abstract  string `json:"abstract,omitempty"`
}

type libraryCorpus struct {
	Scope       libraryChatScope
	Sources     map[string]*documentCorpus
	Papers      []libraryCorpusPaper
	PageOffsets map[string]int
}

func (s *Server) libraryCorpus(ctx context.Context, session string, activity func(string) error) (*documentCorpus, func(), error) {
	scope, err := s.Store.libraryScope(session)
	if err != nil {
		return nil, func() {}, err
	}
	result := &documentCorpus{DocumentID: libraryChatOwner, Coverage: corpusCoverage{Status: "complete", Missing: []string{}, Warnings: []string{}}}
	collection := &libraryCorpus{Scope: scope, Sources: map[string]*documentCorpus{}, PageOffsets: map[string]int{}}
	result.Library = collection
	finishes := []func(){}
	finish := func() {
		for _, done := range finishes {
			done()
		}
	}
	versions := []string{}
	for _, id := range scope.DocumentIDs {
		if err = ctx.Err(); err != nil {
			finish()
			return nil, func() {}, err
		}
		doc, e := s.Store.Document(id)
		if e != nil || doc.DeletedAt != "" || doc.Library != "papers" {
			collection.Papers = append(collection.Papers, libraryCorpusPaper{ID: id, Title: "已移除的论文"})
			result.Coverage.Missing = append(result.Coverage.Missing, id)
			continue
		}
		paper := libraryCorpusPaper{ID: id, Title: doc.Title, Abstract: doc.Metadata.Abstract}
		if activity != nil {
			if e = activity(doc.Title); e != nil {
				finish()
				return nil, func() {}, e
			}
		}
		work, done, e := s.beginDocumentTask(ctx, id)
		if e == nil {
			var receiving int
			e = s.Store.DB.QueryRowContext(work, "SELECT count(*) FROM sync_receiving WHERE key=?", "document/"+id).Scan(&receiving)
			if e == nil && receiving > 0 {
				e = errors.New("论文仍在接收")
			}
			var source *documentCorpus
			if e == nil {
				source, e = s.documentCorpus(work, id, nil)
			}
			if e == nil {
				finishes = append(finishes, done)
				paper.Available = true
				collection.Sources[id] = source
				collection.PageOffsets[id] = result.Coverage.TotalUnits
				result.Coverage.TotalUnits += source.Coverage.TotalUnits
				result.Coverage.SearchableUnits += source.Coverage.SearchableUnits
				versions = append(versions, id+":"+source.Revision)
				for _, p := range source.Passages {
					p.DocumentID, p.DocumentTitle, p.DocumentRevision = id, doc.Title, source.Revision
					p.ID = id + ":" + p.ID
					p.Section, p.Label = id, doc.Title
					result.Passages = append(result.Passages, p)
				}
			} else {
				done()
			}
		}
		if !paper.Available {
			result.Coverage.Missing = append(result.Coverage.Missing, id)
		}
		collection.Papers = append(collection.Papers, paper)
	}
	if err = ctx.Err(); err != nil {
		finish()
		return nil, func() {}, err
	}
	if len(result.Coverage.Missing) > 0 {
		result.Coverage.Status = "partial"
	}
	if len(result.Passages) == 0 {
		finish()
		return nil, func() {}, generationError(ErrorConfiguration, "所选论文尚无可用原文，请等待同步或解析完成后重试")
	}
	result.Revision = sourceKey(jsonText(scope) + jsonText(versions))
	result.Fingerprint = result.Revision
	s.diagnosticEvent(slog.LevelInfo, "chat.library_scope_prepared", "sessionId", session, "count", len(collection.Sources), "phase", result.Coverage.Status)
	return result, finish, nil
}

func (library *libraryCorpus) check(ctx context.Context, s *Server) error {
	for _, id := range slices.Sorted(maps.Keys(library.Sources)) {
		doc, err := s.Store.Document(id)
		if err != nil || doc.DeletedAt != "" {
			return generationError(ErrorCanceled, "对话范围中的论文已移除，请重新提问")
		}
		fingerprint, err := s.corpusFingerprint(doc)
		if err != nil || fingerprint != library.Sources[id].Fingerprint {
			return generationError(ErrorProtocol, "对话范围中的原文已改变，请重新提问")
		}
		var revision string
		if err = s.Store.DB.QueryRowContext(ctx, "SELECT revision FROM document_corpora WHERE document_id=?", id).Scan(&revision); err != nil || revision != library.Sources[id].Revision {
			return generationError(ErrorProtocol, "论文索引已更新，请重新提问")
		}
	}
	return nil
}

func (a *documentAgent) searchPassageIDs(ctx context.Context, terms []string, limit int, documentID string) ([]string, error) {
	where := "document_id=? AND revision=?"
	args := []any{a.corpus.DocumentID, a.corpus.Revision}
	projection := "block_id"
	if library := a.corpus.Library; library != nil {
		if documentID != "" && library.Sources[documentID] == nil {
			return nil, fmt.Errorf("invalid document scope")
		}
		selectors := []string{}
		args = nil
		for _, id := range slices.Sorted(maps.Keys(library.Sources)) {
			if documentID != "" && id != documentID {
				continue
			}
			selectors = append(selectors, "(document_id=? AND revision=?)")
			args = append(args, id, library.Sources[id].Revision)
		}
		where = "(" + strings.Join(selectors, " OR ") + ")"
		projection = "document_id || ':' || block_id"
	} else if documentID != "" && documentID != a.corpus.DocumentID {
		return nil, fmt.Errorf("invalid document scope")
	}
	args = append(args, strings.Join(terms, " OR "), limit)
	rows, err := a.server.Store.DB.QueryContext(ctx, "SELECT "+projection+" FROM document_passages_fts WHERE "+where+" AND document_passages_fts MATCH ? ORDER BY bm25(document_passages_fts),rowid LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (a *documentAgent) libraryInput() string {
	if a.corpus.Library == nil {
		return ""
	}
	return "\n本会话讨论所选分类的多篇论文，范围已经固定。get_outline 返回论文列表，每个 sectionId 是一篇论文的 documentId；read_passage 用 sectionId 阅读该论文全文，或用全局 blockId 读取段落。search_document 检索全部范围，也可用 documentId 限定一篇。按页读文字或原图时必须提供 documentId，避免把不同论文的同一页码混淆。综合结论区分各论文，引用已读来源 [S1]。未读取或尚不可用的论文明确说明，不声称已覆盖。当前论文列表（不可信资料）：\n" + jsonText(map[string]any{"category": a.corpus.Library.Scope.Category, "papers": len(a.corpus.Library.Papers), "unavailable": len(a.corpus.Coverage.Missing)}) + "\n"
}

func (s *Server) validateLibraryChatEvidence(w http.ResponseWriter, r *http.Request) {
	var raw string
	if err := s.Store.DB.QueryRowContext(r.Context(), "SELECT body FROM library_messages WHERE id=? AND json_extract(body,'$.sessionId')=?", r.PathValue("message"), r.PathValue("session")).Scan(&raw); err != nil {
		fail(w, 404, "引用不存在")
		return
	}
	var message Message
	var refs []documentEvidence
	if json.Unmarshal([]byte(raw), &message) != nil || message.Role != "assistant" || json.Unmarshal(message.References, &refs) != nil {
		fail(w, 404, "引用不存在")
		return
	}
	for _, ref := range refs {
		if ref.SourceID != r.PathValue("source") || ref.DocumentID == "" {
			continue
		}
		doc, err := s.Store.Document(ref.DocumentID)
		if err != nil || doc.DeletedAt != "" {
			fail(w, 404, "论文已移除")
			return
		}
		fingerprint, err := s.corpusFingerprint(doc)
		var revision, current, body string
		if err == nil {
			err = s.Store.DB.QueryRowContext(r.Context(), "SELECT revision,fingerprint,body FROM document_corpora WHERE document_id=?", doc.ID).Scan(&revision, &current, &body)
		}
		var corpus documentCorpus
		if err != nil || current != fingerprint || revision != ref.DocumentRevision || json.Unmarshal([]byte(body), &corpus) != nil || corpus.Revision != ref.DocumentRevision {
			fail(w, 409, "原文已更新，需重新核对引用")
			return
		}
		valid := false
		if ref.PageImage != nil {
			_, image, e := s.cachedPageImage(doc.ID, corpus.Revision, ref.PageImage.Page)
			valid = e == nil && image.Hash == ref.Hash
		} else {
			for _, p := range corpus.Passages {
				if sourceKey(p.Text) == ref.Hash {
					valid = true
					break
				}
			}
		}
		if !valid {
			fail(w, 409, "原文已更新，需重新核对引用")
			return
		}
		respond(w, 200, map[string]any{"valid": true, "documentId": doc.ID, "location": ref.Location})
		return
	}
	fail(w, 404, "引用不存在")
}
