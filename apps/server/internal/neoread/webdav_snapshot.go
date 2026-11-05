package neoread

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// This allowlist excludes credentials, provider continuations, undo snapshots,
// derived corpus indexes and running jobs. A receiving device rebuilds those locally.
var syncTables = []string{
	"documents", "tag_boards", "settings", "document_navigation", "annotations",
	"document_notes", "translations", "translation_previous", "document_overviews",
	"paper_attachments", "document_relations", "processing_usage", "chat_usage_coverage",
	"chat_sessions", "messages", "chat_runs",
	"search_index",
}

type syncTable struct{ columns, primary []string }
type syncQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}
type syncSnapshot struct {
	Documents      map[string]*syncDocument
	Observed       map[string]*syncDocument
	Receiving      map[string]*syncReceiving
	ReceivePlans   map[string]*syncReceiving
	Receipts       map[string]*syncCommitReceipt
	State          *syncLocalState
	DeviceName     string
	Files          map[string]syncFingerprint
	Busy           map[string]bool
	Revision       int64
	AppliedChanges int64
}

func syncSchema(ctx context.Context, q syncQuery) (map[string]syncTable, error) {
	result := map[string]syncTable{}
	for _, name := range syncTables {
		rows, err := q.QueryContext(ctx, "PRAGMA table_info("+name+")")
		if err != nil {
			return nil, err
		}
		table := syncTable{}
		primary := map[int]string{}
		for rows.Next() {
			var cid, notnull, pk int
			var column, kind string
			var def any
			if err = rows.Scan(&cid, &column, &kind, &notnull, &def, &pk); err != nil {
				rows.Close()
				return nil, err
			}
			table.columns = append(table.columns, column)
			if pk > 0 {
				primary[pk] = column
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		for n := 1; n <= len(primary); n++ {
			table.primary = append(table.primary, primary[n])
		}
		if name == "search_index" {
			table.primary = []string{"document_id", "href"}
		}
		result[name] = table
	}
	return result, nil
}

func syncRows(ctx context.Context, q syncQuery, name string, table syncTable, owners ...string) ([]syncRow, error) {
	query := "SELECT " + strings.Join(table.columns, ",") + " FROM " + name
	var args []any
	if name == "settings" {
		query += " WHERE key='preferences:library'"
	}
	if len(owners) > 0 {
		documents := []string{}
		library := false
		for _, owner := range owners {
			if owner == "library" {
				library = true
			} else {
				documents = append(documents, strings.TrimPrefix(owner, "document/"))
			}
		}
		if name == "settings" || name == "tag_boards" {
			if !library {
				return []syncRow{}, nil
			}
		} else {
			if len(documents) == 0 {
				return []syncRow{}, nil
			}
			column := "document_id"
			switch name {
			case "documents":
				column = "id"
			case "document_relations":
				column = "a"
			}
			values := strings.TrimSuffix(strings.Repeat("?,", len(documents)), ",")
			if name == "chat_runs" {
				query += " WHERE session_id IN (SELECT id FROM chat_sessions WHERE document_id IN (" + values + "))"
			} else {
				query += " WHERE " + column + " IN (" + values + ")"
			}
			for _, id := range documents {
				args = append(args, id)
			}
		}
	}
	query += " ORDER BY rowid"
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []syncRow{}
	for rows.Next() {
		values := make([]any, len(table.columns))
		targets := make([]any, len(values))
		for i := range targets {
			targets[i] = &values[i]
		}
		if err = rows.Scan(targets...); err != nil {
			return nil, err
		}
		row := syncRow{}
		for i, column := range table.columns {
			if b, ok := values[i].([]byte); ok {
				values[i] = string(b)
			}
			row[column] = values[i]
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
func syncRowKey(table string, row syncRow, schema syncTable) string {
	keys := make([]any, len(schema.primary))
	for i, key := range schema.primary {
		keys[i] = row[key]
	}
	body, _ := json.Marshal(keys)
	return table + "/" + string(body)
}
func emptySyncDocument() *syncDocument {
	return &syncDocument{Records: map[string]syncRecord{}, Files: map[string]syncFile{}}
}

func snapshotSyncRows(ctx context.Context, q syncQuery, owners ...string) (map[string]*syncDocument, error) {
	shared, err := sharedPreferenceScope(ctx, q)
	if err != nil {
		return nil, err
	}
	schema, err := syncSchema(ctx, q)
	if err != nil {
		return nil, err
	}
	result := map[string]*syncDocument{"library": emptySyncDocument()}
	sessionOwners := map[string]string{}
	chats := map[string]syncRecord{}
	for _, table := range syncTables {
		rows, err := syncRows(ctx, q, table, schema[table], owners...)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if table == "settings" {
				if shared {
					var prefs map[string]any
					if json.Unmarshal([]byte(fmtSyncString(row["value"])), &prefs) != nil {
						return nil, errors.New("书库偏好格式无效")
					}
					local := mergeApplicationPreference("preferences:library", prefs, map[string]any{})
					body, _ := json.Marshal(local)
					row["value"] = string(body)
				}
				var preferences map[string]json.RawMessage
				if json.Unmarshal([]byte(fmtSyncString(row["value"])), &preferences) != nil {
					return nil, errors.New("书库偏好格式无效")
				}
				sections := map[string]json.RawMessage{}
				for _, name := range []string{"papers", "books"} {
					if value, ok := preferences[name]; ok {
						if shared {
							var section map[string]any
							if json.Unmarshal(value, &section) == nil && len(section) == 0 {
								continue
							}
						}
						sections[name] = value
					}
				}
				if len(sections) == 0 {
					continue
				}
				body, _ := json.Marshal(sections)
				row["value"] = string(body)
			}
			owner := fmtSyncString(row["document_id"])
			if table == "documents" {
				owner = fmtSyncString(row["id"])
			}
			if table == "document_relations" {
				owner = fmtSyncString(row["a"])
			}
			key := "library"
			if owner != "" {
				key = "document/" + owner
			}
			if table == "chat_runs" {
				owner = sessionOwners[fmtSyncString(row["session_id"])]
				key = "document/" + owner
			}
			if key != "library" && table != "documents" && result[key] == nil {
				continue
			}
			if result[key] == nil {
				result[key] = emptySyncDocument()
			}
			if table == "chat_sessions" || table == "messages" || table == "chat_runs" {
				session := fmtSyncString(row["session_id"])
				if table == "chat_sessions" {
					session = fmtSyncString(row["id"])
					sessionOwners[session] = owner
				}
				if table == "messages" {
					var message struct {
						Session string `json:"sessionId"`
					}
					if json.Unmarshal([]byte(fmtSyncString(row["body"])), &message) != nil {
						return nil, errors.New("本地对话记录无效")
					}
					session = message.Session
				}
				chatKey := key + "/chat/" + session
				if chats[chatKey] == nil {
					chats[chatKey] = syncRecord{}
				}
				chats[chatKey][table] = append(chats[chatKey][table], row)
				result[key].Records["chat/"+session] = chats[chatKey]
				continue
			}
			result[key].Records[syncRowKey(table, row, schema[table])] = syncRecord{table: {row}}
		}
	}
	return result, nil
}

func sharedPreferenceScope(ctx context.Context, q syncQuery) (bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT key FROM settings WHERE key='application:preferences-scope'")
	if err != nil {
		return false, err
	}
	shared := rows.Next()
	err = rows.Err()
	rows.Close()
	return shared, err
}

func (s *Server) syncBusy() map[string]bool {
	s.documentMu.Lock()
	defer s.documentMu.Unlock()
	busy := map[string]bool{}
	for key, tasks := range s.documentTasks {
		if len(tasks) > 0 {
			busy[key] = true
		}
	}
	for key, value := range s.reparsingDocuments {
		if value {
			busy[key] = true
		}
	}
	for key, value := range s.deletingDocuments {
		if value {
			busy[key] = true
		}
	}
	return busy
}
func (s *Server) syncDirectory() string { return filepath.Join(s.Store.Root, ".neoread-sync") }
func (s *Server) syncBlob(hash string) string {
	return filepath.Join(s.syncDirectory(), "objects", hash)
}

func (s *Server) fingerprintSyncFile(ctx context.Context, name string, state *syncLocalState) (syncFingerprint, error) {
	full := filepath.Join(s.Store.Root, filepath.FromSlash(name))
	info, err := os.Lstat(full)
	if err != nil {
		return syncFingerprint{}, err
	}
	if !info.Mode().IsRegular() {
		return syncFingerprint{}, errors.New("同步文件包含链接或特殊文件")
	}
	if cached, ok := state.Files[name]; ok && cached.Size == info.Size() && cached.Modified == info.ModTime().UnixNano() {
		ready := true
		for _, chunk := range cached.File.Chunks {
			if _, err := os.Stat(s.syncBlob(chunk.Hash)); err != nil {
				ready = false
				break
			}
		}
		if ready {
			return cached, nil
		}
	}
	f, err := os.Open(full)
	if err != nil {
		return syncFingerprint{}, err
	}
	defer f.Close()
	value := syncFile{Size: info.Size(), Chunks: []syncChunk{}}
	buffer := make([]byte, syncChunkSize)
	for {
		if err = ctx.Err(); err != nil {
			return syncFingerprint{}, err
		}
		n, readErr := io.ReadFull(f, buffer)
		if n > 0 {
			body := buffer[:n]
			hash := syncBytesHash(body)
			if err = atomicFile(s.syncBlob(hash), body); err != nil {
				return syncFingerprint{}, err
			}
			value.Chunks = append(value.Chunks, syncChunk{Hash: hash, Size: n})
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return syncFingerprint{}, readErr
		}
	}
	after, err := f.Stat()
	if err != nil {
		return syncFingerprint{}, err
	}
	if after.Size() != info.Size() || after.ModTime() != info.ModTime() {
		return syncFingerprint{}, errors.New("同步时文件发生变化，请稍后重试")
	}
	return syncFingerprint{Size: info.Size(), Modified: info.ModTime().UnixNano(), File: value}, nil
}

func (s *Server) snapshotSync(ctx context.Context, state *syncLocalState) (*syncSnapshot, error) {
	s.libraryArchiveMu.Lock()
	defer s.libraryArchiveMu.Unlock()
	tx, err := s.Store.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	documents, err := snapshotSyncRows(ctx, tx)
	var receiving map[string]*syncReceiving
	var receipts map[string]*syncCommitReceipt
	if err == nil {
		receiving, err = readSyncReceiving(ctx, tx)
	}
	if err == nil {
		receipts, err = readSyncReceipts(ctx, tx)
	}
	var revision int64
	if err == nil {
		revision, err = syncLocalRevision(ctx, tx)
	}
	tx.Rollback()
	if err != nil {
		return nil, err
	}
	result := &syncSnapshot{Documents: documents, Observed: maps.Clone(documents), Receiving: receiving, Files: map[string]syncFingerprint{}, Busy: s.syncBusy(), Revision: revision}
	for key, doc := range documents {
		if key == "library" {
			continue
		}
		owner := strings.TrimPrefix(key, "document/")
		if !safeSyncID(owner) {
			return nil, errors.New("本地文档标识无效")
		}
		if result.Busy[owner] {
			if old := state.Base[key]; old != nil {
				result.Documents[key] = old
			} else {
				delete(result.Documents, key)
			}
			continue
		}
		addFile := func(name string) error {
			fingerprint, err := s.fingerprintSyncFile(ctx, name, state)
			if err != nil {
				return err
			}
			doc.Files[name] = fingerprint.File
			result.Files[name] = fingerprint
			return nil
		}
		var documentRow syncRow
		for _, record := range doc.Records {
			if len(record["documents"]) == 1 {
				documentRow = record["documents"][0]
				break
			}
		}
		if documentRow == nil {
			return nil, errors.New("本地文档记录不完整")
		}
		kind := fmtSyncString(documentRow["type"])
		directory := "books"
		if kind == "pdf" {
			directory = "papers"
		}
		if err = addFile(directory + "/" + owner + "." + kind); err != nil {
			return nil, err
		}
		cache := filepath.Join(s.Store.Root, "cache", owner)
		err = filepath.WalkDir(cache, func(full string, entry fs.DirEntry, walkErr error) error {
			if os.IsNotExist(walkErr) && full == cache {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			relative, _ := filepath.Rel(cache, full)
			parts := strings.Split(filepath.ToSlash(relative), "/")
			if parts[0] == "translation-failures" || strings.HasSuffix(parts[0], "-work") {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			return addFile("cache/" + owner + "/" + filepath.ToSlash(relative))
		})
		if err != nil {
			return nil, err
		}
		for _, record := range doc.Records {
			for _, attachment := range record["paper_attachments"] {
				if fmtSyncString(attachment["kind"]) == "file" {
					if err = addFile("attachments/" + fmtSyncString(attachment["sha256"])); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if err = result.restoreReceiving(state); err != nil {
		return nil, err
	}
	for key, receipt := range receipts {
		applySyncReceipt(state, key, receipt)
	}
	return result, nil
}

func validSyncFile(name, owner string) bool {
	if strings.ContainsAny(name, "\\\x00") || path.Clean(name) != name || strings.HasPrefix(name, "/") {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) == 2 && parts[0] == "attachments" {
		decoded, err := hex.DecodeString(parts[1])
		return err == nil && len(decoded) == 32
	}
	if len(parts) == 2 && (name == "papers/"+owner+".pdf" || name == "books/"+owner+".epub" || name == "books/"+owner+".pdf") {
		return true
	}
	if len(parts) < 3 || parts[0] != "cache" || parts[1] != owner {
		return false
	}
	for _, part := range parts {
		if !safeSyncID(part) {
			return false
		}
	}
	return parts[2] != "translation-failures" && !strings.HasSuffix(parts[2], "-work")
}

func validateSyncDocument(key string, doc *syncDocument, schema map[string]syncTable) error {
	if doc == nil || doc.Records == nil || doc.Files == nil {
		return errors.New("云端文档内容不完整")
	}
	owner := strings.TrimPrefix(key, "document/")
	hasDocument := key == "library"
	for recordKey, record := range doc.Records {
		if len(record) == 0 {
			return errors.New("云端同步记录为空")
		}
		for name, rows := range record {
			table, ok := schema[name]
			if !ok {
				return errors.New("云端同步包含不支持的记录类型")
			}
			for _, row := range rows {
				if len(row) != len(table.columns) {
					return errors.New("云端同步记录版本不兼容")
				}
				for _, column := range table.columns {
					if _, ok := row[column]; !ok {
						return errors.New("云端同步字段不完整")
					}
					if _, err := syncSQLValue(row[column]); err != nil {
						return err
					}
				}
				jsonColumns := []string{}
				switch name {
				case "documents":
					jsonColumns = []string{"metadata", "tags", "folders", "progress", "body_end"}
				case "annotations", "messages", "chat_runs", "translations", "translation_previous", "document_navigation", "document_overviews", "processing_usage":
					jsonColumns = []string{"body"}
				case "settings":
					jsonColumns = []string{"value"}
				}
				for _, column := range jsonColumns {
					if row[column] != nil && fmtSyncString(row[column]) != "" && !json.Valid([]byte(fmtSyncString(row[column]))) {
						return errors.New("云端记录包含无效的结构化内容")
					}
				}
				if key == "library" {
					if name != "settings" && name != "tag_boards" {
						return errors.New("云端书库配置包含无效内容")
					}
					if name == "settings" && row["key"] != "preferences:library" {
						return errors.New("云端同步包含机器配置")
					}
					if name == "settings" {
						var sections map[string]json.RawMessage
						if json.Unmarshal([]byte(fmtSyncString(row["value"])), &sections) != nil || len(sections) == 0 || len(sections) > 2 || sections["papers"] == nil && sections["books"] == nil {
							return errors.New("云端设置不属于当前书库")
						}
						for section := range sections {
							if section != "papers" && section != "books" {
								return errors.New("云端同步包含机器配置")
							}
						}
					}
				} else {
					rowOwner := fmtSyncString(row["document_id"])
					if name == "documents" {
						if !validLibrary(fmtSyncString(row["library"])) {
							return errors.New("云端目录属于其他书库，请选择独立文件夹")
						}
						rowOwner = fmtSyncString(row["id"])
						hasDocument = true
						kind := fmtSyncString(row["type"])
						original := "books/" + owner + "." + kind
						if kind == "pdf" {
							original = "papers/" + owner + ".pdf"
						}
						if kind != "pdf" && kind != "epub" {
							return errors.New("云端文档类型无效")
						}
						if _, ok := doc.Files[original]; !ok {
							return errors.New("云端文档缺少原文件")
						}
					}
					if name == "document_relations" {
						rowOwner = fmtSyncString(row["a"])
					}
					if name == "chat_runs" {
						var run struct {
							Document string `json:"documentId"`
						}
						if json.Unmarshal([]byte(fmtSyncString(row["body"])), &run) != nil {
							return errors.New("云端对话状态无效")
						}
						rowOwner = run.Document
					}
					if rowOwner != owner {
						return errors.New("云端记录不属于该文档")
					}
				}
			}
			if !strings.HasPrefix(recordKey, "chat/") && (len(record) != 1 || len(rows) != 1 || syncRowKey(name, rows[0], table) != recordKey) {
				return errors.New("云端同步记录标识不一致")
			}
			if strings.HasPrefix(recordKey, "chat/") && !slices.Contains([]string{"chat_sessions", "messages", "chat_runs"}, name) {
				return errors.New("云端对话包含无效记录")
			}
		}
	}
	if !hasDocument {
		return errors.New("云端文档信息缺失")
	}
	for name, file := range doc.Files {
		if key == "library" || !validSyncFile(name, owner) || file.Size < 0 {
			return errors.New("云端文件路径无效")
		}
		var size int64
		for _, chunk := range file.Chunks {
			hash, err := hex.DecodeString(chunk.Hash)
			if err != nil || len(hash) != 32 || chunk.Size <= 0 || chunk.Size > syncChunkSize {
				return errors.New("云端文件分块无效")
			}
			size += int64(chunk.Size)
		}
		if size != file.Size {
			return errors.New("云端文件大小不一致")
		}
	}
	return nil
}
