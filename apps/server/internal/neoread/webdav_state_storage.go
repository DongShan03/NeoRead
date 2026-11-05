package neoread

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

// The small root is the commit point. A changed document gets a new immutable
// record; unchanged documents are not rewritten. A crash before replacing the
// root leaves its previous records intact.
type syncStoredRoot struct {
	Format    string                     `json:"format"`
	Version   int                        `json:"version"`
	Device    string                     `json:"device"`
	Clock     syncClock                  `json:"clock"`
	Documents map[string]string          `json:"documents"`
	Remotes   map[string]syncRemoteCache `json:"remotes"`
	LastSync  string                     `json:"lastSync,omitempty"`
}

type syncStoredDocument struct {
	Own       *syncVersion               `json:"own,omitempty"`
	Base      *syncDocument              `json:"base,omitempty"`
	BaseClock syncClock                  `json:"baseClock,omitempty"`
	Choice    *syncChoice                `json:"choice,omitempty"`
	Files     map[string]syncFingerprint `json:"files,omitempty"`
}

func syncStoredRecordPath(root, key, hash string) (string, error) {
	if !syncCacheHash(hash) {
		return "", errors.New("同步文档记录引用无效")
	}
	directory := "library"
	if key != "library" {
		if !strings.HasPrefix(key, "document/") || !safeSyncID(strings.TrimPrefix(key, "document/")) {
			return "", errors.New("同步文档记录归属无效")
		}
		directory = filepath.Join("documents", strings.TrimPrefix(key, "document/"))
	}
	return filepath.Join(filepath.Dir(root), directory, "state", hash+".json"), nil
}

func readSyncStoredState(root *os.Root, name string) (*syncLocalState, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("同步状态不是普通文件")
	}
	body, err := root.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var header struct {
		Format  string `json:"format"`
		Version int    `json:"version"`
	}
	if json.Unmarshal(body, &header) != nil {
		return nil, errors.New("同步状态无法读取")
	}
	if header.Format == "" && header.Version == 0 {
		var state syncLocalState
		if decodeSyncJSON(body, &state) != nil {
			return nil, errors.New("旧版同步状态无法读取")
		}
		state.LegacyStorage = true
		return &state, nil
	}
	var stored syncStoredRoot
	if decodeSyncJSON(body, &stored) != nil || stored.Format != "neoread-sync-state" || stored.Version != 2 || !syncID(stored.Device) || stored.Clock == nil || stored.Documents == nil || stored.Remotes == nil {
		return nil, errors.New("本地同步状态版本无效，已保留书库数据")
	}
	state := newSyncState()
	state.Device, state.Clock, state.Remotes, state.LastSync = stored.Device, stored.Clock, stored.Remotes, stored.LastSync
	state.StoredRecords = stored.Documents
	for key, hash := range stored.Documents {
		file, err := syncStoredRecordPath(name, key, hash)
		if err != nil {
			return nil, err
		}
		info, err := root.Lstat(file)
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("同步文档状态缺失，已保留书库数据")
		}
		body, err := root.ReadFile(file)
		if err != nil || syncBytesHash(body) != hash {
			return nil, errors.New("同步文档状态校验失败，已保留书库数据")
		}
		var document syncStoredDocument
		if decodeSyncJSON(body, &document) != nil {
			return nil, errors.New("同步文档状态无法读取")
		}
		if document.Own != nil {
			state.Own[key] = document.Own
		}
		if document.Base != nil {
			state.Base[key] = document.Base
		}
		if document.BaseClock != nil {
			state.BaseClocks[key] = document.BaseClock
		}
		if document.Choice != nil {
			state.Choices[key] = *document.Choice
		}
		for name, fingerprint := range document.Files {
			state.Files[name] = fingerprint
		}
	}
	return state, nil
}

func buildSyncStoredDocument(state *syncLocalState, key string) *syncStoredDocument {
	_, hasOwn := state.Own[key]
	_, hasBase := state.Base[key]
	_, hasClock := state.BaseClocks[key]
	choice, hasChoice := state.Choices[key]
	if !hasOwn && !hasBase && !hasClock && !hasChoice {
		return nil
	}
	record := &syncStoredDocument{Own: state.Own[key], Base: state.Base[key], BaseClock: state.BaseClocks[key], Files: map[string]syncFingerprint{}}
	if hasChoice {
		record.Choice = &choice
	}
	bodies := []*syncDocument{record.Base}
	if record.Own != nil {
		bodies = append(bodies, record.Own.Body)
	}
	for _, body := range bodies {
		if body != nil {
			for name := range body.Files {
				if fingerprint, ok := state.Files[name]; ok {
					record.Files[name] = fingerprint
				}
			}
		}
	}
	return record
}

func syncStoredDocuments(state *syncLocalState) map[string]*syncStoredDocument {
	keys := map[string]bool{}
	for key := range state.Own {
		keys[key] = true
	}
	for key := range state.Base {
		keys[key] = true
	}
	for key := range state.BaseClocks {
		keys[key] = true
	}
	for key := range state.Choices {
		keys[key] = true
	}
	result := map[string]*syncStoredDocument{}
	for key := range keys {
		result[key] = buildSyncStoredDocument(state, key)
	}
	return result
}

func (s *Server) writeSyncStoredState(config webdavConfig, state *syncLocalState) error {
	if err := writeSyncStoredStateAt(s.syncStatePath(config), state, nil); err != nil {
		return err
	}
	return s.clearSyncReceipts(state.Connection, nil)
}

func (s *Server) saveSyncDocumentState(config webdavConfig, state *syncLocalState, key string) error {
	if err := writeSyncStoredStateAt(s.syncStatePath(config), state, &key); err != nil {
		return err
	}
	return s.clearSyncReceipts(state.Connection, &key)
}

func writeSyncStoredStateAt(name string, state *syncLocalState, only *string) error {
	if state.LegacyStorage {
		body, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		backup := filepath.Join(filepath.Dir(name), "state-v1.backup.json")
		if _, err = os.Stat(backup); os.IsNotExist(err) {
			if err = atomicFile(backup, body); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		// The legacy format also persisted unpublished uploads in its large
		// state. Preserve them in the existing append-only recovery log.
		file, err := os.OpenFile(filepath.Join(filepath.Dir(name), "uploads.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		writer := bufio.NewWriter(file)
		for hash, uploaded := range state.Uploaded {
			if uploaded {
				if _, err = writer.WriteString(hash + "\n"); err != nil {
					break
				}
			}
		}
		if err == nil {
			err = writer.Flush()
		}
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	stored := syncStoredRoot{Format: "neoread-sync-state", Version: 2, Device: state.Device, Clock: state.Clock, Documents: map[string]string{}, Remotes: state.Remotes, LastSync: state.LastSync}
	documents := map[string]*syncStoredDocument{}
	if only != nil && state.StoredRecords != nil && !state.LegacyStorage {
		stored.Documents = maps.Clone(state.StoredRecords)
		if document := buildSyncStoredDocument(state, *only); document != nil {
			documents = map[string]*syncStoredDocument{*only: document}
		} else {
			delete(stored.Documents, *only)
			documents = map[string]*syncStoredDocument{}
		}
	} else {
		documents = syncStoredDocuments(state)
	}
	for key, document := range documents {
		body, err := json.Marshal(document)
		if err != nil {
			return err
		}
		hash := syncBytesHash(body)
		file, err := syncStoredRecordPath(name, key, hash)
		if err != nil {
			return err
		}
		info, statErr := os.Lstat(file)
		if state.StoredRecords[key] != hash || statErr != nil || !info.Mode().IsRegular() || info.Size() != int64(len(body)) {
			if err = atomicFile(file, body); err != nil {
				return err
			}
		}
		stored.Documents[key] = hash
	}
	body, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if err = atomicFile(name, body); err != nil {
		return err
	}
	state.LegacyStorage = false
	state.StoredRecords = maps.Clone(stored.Documents)
	return nil
}

func pruneStoredSyncRecords(root *os.Root, name string, state *syncLocalState) (int, int64, error) {
	if state.StoredRecords == nil { // Legacy states do not reference record files.
		return 0, 0, nil
	}
	live := map[string]bool{}
	for key, hash := range state.StoredRecords {
		file, err := syncStoredRecordPath(name, key, hash)
		if err != nil {
			return 0, 0, err
		}
		live[file] = true
	}
	directories := []string{filepath.Join(filepath.Dir(name), "library", "state")}
	documents := filepath.Join(filepath.Dir(name), "documents")
	entries, err := root.Open(documents)
	if err == nil {
		items, readErr := entries.ReadDir(-1)
		entries.Close()
		if readErr != nil {
			return 0, 0, readErr
		}
		for _, item := range items {
			if item.IsDir() && safeSyncID(item.Name()) {
				directories = append(directories, filepath.Join(documents, item.Name(), "state"))
			}
		}
	} else if !os.IsNotExist(err) {
		return 0, 0, err
	}
	count, bytes := 0, int64(0)
	for _, directory := range directories {
		folder, err := root.Open(directory)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return count, bytes, err
		}
		items, err := folder.ReadDir(-1)
		folder.Close()
		if err != nil {
			return count, bytes, err
		}
		for _, item := range items {
			file := filepath.Join(directory, item.Name())
			if live[file] || !strings.HasSuffix(item.Name(), ".json") || !syncCacheHash(strings.TrimSuffix(item.Name(), ".json")) {
				continue
			}
			info, err := item.Info()
			if err != nil {
				return count, bytes, err
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if err = root.Remove(file); err != nil {
				return count, bytes, err
			}
			count++
			bytes += info.Size()
		}
	}
	return count, bytes, nil
}

// Upgrade local synchronization metadata on opening a library, including paused
// connections. Original files and SQLite user records are never moved here.
func migrateLocalSyncStorage(directory string) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return
	}
	defer root.Close()
	identities, err := os.ReadDir(filepath.Join(directory, ".neoread-sync"))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		slog.Warn("sync.storage_migration_failed", "phase", "local", "errorCode", "read")
		return
	}
	for _, identity := range identities {
		if !identity.IsDir() || !syncCacheHash(identity.Name()) {
			continue
		}
		name := filepath.Join(".neoread-sync", identity.Name(), "state.json")
		state, err := readSyncStoredState(root, name)
		if os.IsNotExist(err) {
			continue
		}
		if err == nil && !state.LegacyStorage {
			continue
		}
		if err == nil && (!syncID(state.Device) || state.Clock == nil || state.Own == nil || state.Base == nil || state.BaseClocks == nil || state.Files == nil || state.Uploaded == nil || state.Remotes == nil || state.Choices == nil) {
			err = errors.New("旧版同步状态不完整")
		}
		if err == nil {
			err = writeSyncStoredStateAt(filepath.Join(directory, name), state, nil)
		}
		if err != nil {
			slog.Warn("sync.storage_migration_failed", "phase", "local", "errorCode", "state")
		} else {
			slog.Info("sync.storage_migrated", "phase", "local", "count", len(state.StoredRecords))
		}
	}
}
