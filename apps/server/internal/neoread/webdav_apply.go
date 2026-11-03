package neoread

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type syncInstallFile struct {
	Name    string `json:"name"`
	Existed bool   `json:"existed"`
	Remove  bool   `json:"remove,omitempty"`
}
type syncInstallJournal struct {
	ID    string            `json:"id"`
	Files []syncInstallFile `json:"files"`
}

// The journal and a marker committed with the SQLite writes make file
// replacement recoverable across a crash at either side of the DB commit.
func (s *Server) recoverSyncInstall() error {
	journalPath := filepath.Join(s.syncDirectory(), "apply.json")
	body, err := os.ReadFile(journalPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var journal syncInstallJournal
	if decodeSyncJSON(body, &journal) != nil || !syncID(journal.ID) {
		return errors.New("同步恢复记录无效")
	}
	var committed string
	err = s.Store.DB.QueryRow("SELECT value FROM settings WHERE key='sync:apply'").Scan(&committed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	root, err := os.OpenRoot(s.Store.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, file := range journal.Files {
		if filepath.IsAbs(file.Name) || filepath.ToSlash(filepath.Clean(file.Name)) != file.Name || strings.HasPrefix(file.Name, "..") {
			return errors.New("同步恢复路径无效")
		}
		if committed == journal.ID {
			continue
		}
		backup := filepath.Join(".neoread-sync", "apply", journal.ID, "backup", file.Name)
		staged := filepath.Join(".neoread-sync", "apply", journal.ID, "staged", file.Name)
		if file.Existed {
			if _, e := root.Stat(backup); e == nil {
				if e = root.Remove(file.Name); e != nil && !os.IsNotExist(e) {
					return e
				}
				if e = root.Rename(backup, file.Name); e != nil {
					return e
				}
			} else if !os.IsNotExist(e) {
				return e
			}
		} else if _, e := root.Stat(staged); os.IsNotExist(e) {
			if e = root.Remove(file.Name); e != nil && !os.IsNotExist(e) {
				return e
			}
		}
	}
	if err = os.RemoveAll(filepath.Join(s.syncDirectory(), "apply", journal.ID)); err != nil {
		return err
	}
	return os.Remove(journalPath)
}

func (s *Server) stageSyncDocument(ctx context.Context, dav *davClient, run string, before, doc *syncDocument) error {
	for name, file := range doc.Files {
		if before != nil {
			if old, ok := before.Files[name]; ok && syncHash(old) == syncHash(file) {
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		target := filepath.Join(s.syncDirectory(), "apply", run, "staged", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		for _, chunk := range file.Chunks {
			body, readErr := os.ReadFile(s.syncBlob(chunk.Hash))
			if readErr != nil || len(body) != chunk.Size || syncBytesHash(body) != chunk.Hash {
				body, err = dav.get(ctx, "objects/"+chunk.Hash[:2]+"/"+chunk.Hash, syncChunkSize)
				if err == nil && (len(body) != chunk.Size || syncBytesHash(body) != chunk.Hash) {
					err = errors.New("云端文件校验失败，保留本地版本")
				}
				if err == nil {
					err = atomicFile(s.syncBlob(chunk.Hash), body)
				}
				if err != nil {
					out.Close()
					return err
				}
				s.webdav.mu.Lock()
				s.webdav.status.Transferred += len(body)
				s.webdav.mu.Unlock()
			}
			if _, err = out.Write(body); err != nil {
				out.Close()
				return err
			}
		}
		if err = out.Sync(); err != nil {
			out.Close()
			return err
		}
		if err = out.Close(); err != nil {
			return err
		}
		if strings.HasPrefix(name, "cache/") {
			probe, e := os.Open(target)
			if e != nil {
				return e
			}
			prefix := make([]byte, 1024)
			n, _ := probe.Read(prefix)
			probe.Close()
			prefix = bytes.TrimSpace(bytes.TrimPrefix(prefix[:n], []byte{0xef, 0xbb, 0xbf}))
			extension := strings.ToLower(filepath.Ext(name))
			if bytes.HasPrefix(prefix, []byte("<")) || slices.Contains([]string{".xhtml", ".html", ".htm", ".svg"}, extension) {
				if file.Size > 64<<20 {
					return errors.New("书籍单个排版文件过大，未应用云端内容")
				}
				content, e := os.ReadFile(target)
				if e != nil {
					return e
				}
				cleaned, _, e := sanitizeContent(content)
				if e != nil {
					return errors.New("无法清理同步书籍中的活动内容")
				}
				if e = os.WriteFile(target, cleaned, 0600); e != nil {
					return e
				}
			}
		}
	}
	return nil
}

func syncSQLValue(value any) (any, error) {
	if number, ok := value.(json.Number); ok {
		if n, err := number.Int64(); err == nil {
			return n, nil
		}
		return number.Float64()
	}
	switch value.(type) {
	case nil, string, int64, float64:
		return value, nil
	}
	return nil, errors.New("同步记录包含无效的数据库值")
}
func execSyncRow(ctx context.Context, tx *sql.Tx, name string, schema syncTable, row syncRow, remove bool) error {
	args := []any{}
	if remove {
		where := []string{}
		for _, column := range schema.primary {
			value, err := syncSQLValue(row[column])
			if err != nil {
				return err
			}
			where = append(where, column+"=?")
			args = append(args, value)
		}
		if name == "settings" {
			var previous string
			if err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='preferences:library'").Scan(&previous); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			var old, sections map[string]json.RawMessage
			_ = json.Unmarshal([]byte(previous), &old)
			_ = json.Unmarshal([]byte(fmtSyncString(row["value"])), &sections)
			for key := range sections {
				delete(old, key)
			}
			if len(old) > 0 {
				body, _ := json.Marshal(old)
				_, err := tx.ExecContext(ctx, "UPDATE settings SET value=? WHERE key='preferences:library'", string(body))
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM "+name+" WHERE "+strings.Join(where, " AND "), args...)
		return err
	}
	if name == "search_index" {
		if err := execSyncRow(ctx, tx, name, schema, row, true); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO search_index(document_id,href,content) VALUES(?,?,?)", row["document_id"], row["href"], row["content"])
		return err
	}
	if name == "settings" {
		var incoming map[string]json.RawMessage
		if json.Unmarshal([]byte(fmtSyncString(row["value"])), &incoming) != nil {
			return errors.New("同步书库设置无效")
		}
		var previous string
		err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='preferences:library'").Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		merged := map[string]json.RawMessage{}
		if previous != "" && json.Unmarshal([]byte(previous), &merged) != nil {
			return errors.New("本地书库设置无效")
		}
		for key, value := range incoming {
			merged[key] = value
		}
		body, _ := json.Marshal(merged)
		copy := syncRow{}
		for key, value := range row {
			copy[key] = value
		}
		copy["value"] = string(body)
		row = copy
	}
	for _, column := range schema.columns {
		value, err := syncSQLValue(row[column])
		if err != nil {
			return err
		}
		args = append(args, value)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(schema.columns)), ",")
	updates := []string{}
	for _, column := range schema.columns {
		if !slices.Contains(schema.primary, column) {
			updates = append(updates, column+"=excluded."+column)
		}
	}
	conflict := "DO NOTHING"
	if len(updates) > 0 {
		conflict = "DO UPDATE SET " + strings.Join(updates, ",")
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO "+name+"("+strings.Join(schema.columns, ",")+") VALUES("+placeholders+") ON CONFLICT("+strings.Join(schema.primary, ",")+") "+conflict, args...)
	if err == nil && name == "document_overviews" {
		var overview documentOverview
		if json.Unmarshal([]byte(fmtSyncString(row["body"])), &overview) != nil {
			return errors.New("云端文档概要无效")
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM document_overviews_fts WHERE document_id=?", row["document_id"]); err == nil && overview.Content != nil {
			text := overview.Content.Summary + "\n" + strings.Join(overview.Content.Terms, " ")
			_, err = tx.ExecContext(ctx, "INSERT INTO document_overviews_fts(document_id,revision,content) VALUES(?,?,?)", row["document_id"], overview.Revision, text)
		}
	}
	return err
}

func applySyncRecords(ctx context.Context, tx *sql.Tx, key string, before, after *syncDocument, schema map[string]syncTable) error {
	if after == nil {
		owner := strings.TrimPrefix(key, "document/")
		for _, name := range archiveFTSTables {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+name+" WHERE document_id=?", owner); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM documents WHERE id=?", owner)
		return err
	}
	if before == nil {
		before = emptySyncDocument()
	}
	// Deletions run children-first; transcript changes replace one session's
	// ordered messages as a unit, never all conversations in a document.
	order := slices.Clone(syncTables)
	slices.Reverse(order)
	for _, name := range order {
		for recordKey, record := range before.Records {
			if syncHash(record) == syncHash(after.Records[recordKey]) {
				continue
			}
			for _, row := range record[name] {
				if name == "documents" {
					continue
				}
				if err := execSyncRow(ctx, tx, name, schema[name], row, true); err != nil {
					return err
				}
			}
		}
	}
	for _, name := range syncTables {
		for recordKey, record := range after.Records {
			if syncHash(record) == syncHash(before.Records[recordKey]) {
				continue
			}
			for _, row := range record[name] {
				if name == "document_relations" {
					var count int
					if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM documents WHERE id IN (?,?)", row["a"], row["b"]).Scan(&count); err != nil {
						return err
					}
					if count != 2 {
						continue
					} // Deleting a document also removes its relations.
				}
				if err := execSyncRow(ctx, tx, name, schema[name], row, false); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Processing jobs are device-local. Reconstruct the readable state from synced
// assets, without restarting paid translation work on the receiving device.
func (s *Server) applySyncProcessing(ctx context.Context, tx *sql.Tx, key string, before, after *syncDocument) error {
	if key == "library" || after == nil {
		return nil
	}
	owner := strings.TrimPrefix(key, "document/")
	var existing int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM document_processing WHERE document_id=?", owner).Scan(&existing); err != nil {
		return err
	}
	manifest := "cache/" + owner + "/analysis/manifest.json"
	old, _ := before.FileSafe(manifest)
	current, _ := after.FileSafe(manifest)
	if existing > 0 && syncHash(old) == syncHash(current) {
		return nil
	}
	var d Document
	if err := tx.QueryRowContext(ctx, "SELECT id,type,library FROM documents WHERE id=?", owner).Scan(&d.ID, &d.Type, &d.Library); err != nil {
		return err
	}
	p := initialDocumentProcessing(d, `{"autoTranslateMaxPages":0}`)
	if d.Type == "pdf" {
		m, err := s.readLayout(owner)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			p.PagesDone, p.PagesTotal = m.Pages, m.Pages
			saved := map[string]bool{}
			for _, record := range after.Records {
				for _, row := range record["translations"] {
					if row["status"] == "complete" {
						saved[fmtSyncString(row["block_id"])+":"+fmtSyncString(row["source_hash"])] = true
					}
				}
			}
			for _, item := range m.translationItems() {
				p.TranslationsTotal++
				if saved[item.BlockID+":"+item.SourceHash] {
					p.TranslationsDone++
				}
			}
			p.Translating = &ProcessingStage{Status: "complete", Detail: "已同步"}
			if p.TranslationsDone < p.TranslationsTotal {
				p.Enabled = false
				p.ManualTranslation = true
				p.Translating = &ProcessingStage{Status: "paused", Detail: "等待手动翻译"}
			}
			p.Phase = "translating"
			aggregateProcessing(&p)
		}
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO document_processing(document_id,phase,status,body) VALUES(?,?,?,?) ON CONFLICT(document_id) DO UPDATE SET phase=excluded.phase,status=excluded.status,body=excluded.body", owner, p.Phase, p.Status, string(body))
	return err
}

func (s *Server) installSync(ctx context.Context, run string, initial *syncSnapshot, updates map[string]*syncDocument) (map[string]bool, error) {
	if len(updates) == 0 {
		return map[string]bool{}, nil
	}
	s.libraryArchiveMu.Lock()
	defer s.libraryArchiveMu.Unlock()
	s.annotationMu.Lock()
	defer s.annotationMu.Unlock()
	s.documentMu.Lock()
	defer s.documentMu.Unlock()
	tx, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); err != nil {
		return nil, err
	}
	beforeRevision, err := syncLocalRevision(ctx, tx)
	if err != nil {
		return nil, err
	}
	current, err := snapshotSyncRows(ctx, tx)
	if err != nil {
		return nil, err
	}
	schema, err := syncSchema(ctx, tx)
	if err != nil {
		return nil, err
	}
	accepted := map[string]bool{}
	planned := map[string]bool{}
	journal := syncInstallJournal{ID: run, Files: []syncInstallFile{}}
	root, err := os.OpenRoot(s.Store.Root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	for key, after := range updates {
		owner := strings.TrimPrefix(key, "document/")
		if key != "library" && (len(s.documentTasks[owner]) > 0 || s.reparsingDocuments[owner] || s.deletingDocuments[owner]) {
			continue
		}
		if syncHash(current[key].RecordsSafe()) != syncHash(initial.Documents[key].RecordsSafe()) {
			continue
		}
		unchanged := true
		if before := initial.Documents[key]; before != nil {
			for name := range before.Files {
				info, e := root.Lstat(name)
				fingerprint := initial.Files[name]
				if e != nil || !info.Mode().IsRegular() || info.Size() != fingerprint.Size || info.ModTime().UnixNano() != fingerprint.Modified {
					unchanged = false
					break
				}
			}
		}
		if !unchanged {
			continue
		}
		accepted[key] = true
		names := map[string]bool{}
		if after != nil {
			for name, file := range after.Files {
				old, ok := initial.Documents[key].FileSafe(name)
				if !ok || syncHash(old) != syncHash(file) {
					names[name] = false
				}
			}
		}
		if initial.Documents[key] != nil {
			for name := range initial.Documents[key].Files {
				if _, ok := after.FileSafe(name); !ok {
					names[name] = true
				}
			}
		}
		for name, remove := range names {
			if planned[name] {
				continue
			}
			planned[name] = true
			info, e := root.Lstat(name)
			if e != nil && !os.IsNotExist(e) {
				return nil, e
			}
			if e == nil && !info.Mode().IsRegular() {
				return nil, errors.New("同步目标包含链接或特殊文件")
			}
			if err = root.MkdirAll(filepath.Dir(name), 0700); err != nil {
				return nil, err
			}
			if err = root.MkdirAll(filepath.Join(".neoread-sync", "apply", run, "backup", filepath.Dir(name)), 0700); err != nil {
				return nil, err
			}
			journal.Files = append(journal.Files, syncInstallFile{Name: name, Existed: e == nil, Remove: remove})
		}
	}
	// Do not modify files before a durable recovery plan exists.
	body, _ := json.Marshal(journal)
	if err = atomicFile(filepath.Join(s.syncDirectory(), "apply.json"), body); err != nil {
		return nil, err
	}
	for _, file := range journal.Files {
		if file.Existed {
			if err = root.Rename(file.Name, filepath.Join(".neoread-sync", "apply", run, "backup", file.Name)); err != nil {
				break
			}
		}
		if file.Remove {
			continue
		}
		err = root.Rename(filepath.Join(".neoread-sync", "apply", run, "staged", file.Name), file.Name)
		if err != nil {
			break
		}
	}
	// Insert all parents before applying cross-document relations.
	if err == nil {
		for key := range accepted {
			if after := updates[key]; after != nil {
				for _, record := range after.Records {
					for _, row := range record["documents"] {
						if err = execSyncRow(ctx, tx, "documents", schema["documents"], row, false); err != nil {
							break
						}
					}
				}
			}
			if err != nil {
				break
			}
		}
	}
	if err == nil {
		for key := range accepted {
			if err = applySyncRecords(ctx, tx, key, current[key], updates[key], schema); err != nil {
				break
			}
			if err = s.applySyncProcessing(ctx, tx, key, current[key], updates[key]); err != nil {
				break
			}
		}
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('sync:apply',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", run)
	}
	var afterRevision int64
	if err == nil {
		afterRevision, err = syncLocalRevision(ctx, tx)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		tx.Rollback()
		if recoveryErr := s.recoverSyncInstall(); recoveryErr != nil {
			return nil, errors.New("同步未完成，文件恢复将在下次启动时继续")
		}
		return nil, err
	}
	initial.AppliedChanges = afterRevision - beforeRevision
	return accepted, nil
}

func (d *syncDocument) RecordsSafe() map[string]syncRecord {
	if d == nil {
		return nil
	}
	return d.Records
}

func (d *syncDocument) FileSafe(name string) (syncFile, bool) {
	if d == nil {
		return syncFile{}, false
	}
	v, ok := d.Files[name]
	return v, ok
}
