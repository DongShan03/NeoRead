package neoread

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type webdavVariant struct {
	ID      string `json:"id"`
	Device  string `json:"device"`
	Updated string `json:"updated"`
	Deleted bool   `json:"deleted"`
	Preview string `json:"preview"`
}
type webdavConflict struct {
	Key         string          `json:"key"`
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Versions    []webdavVariant `json:"versions"`
	CanKeepBoth bool            `json:"canKeepBoth"`
}
type webdavStatus struct {
	RunID       string           `json:"runId,omitempty"`
	Manual      bool             `json:"manual"`
	Dirty       bool             `json:"dirty"`
	Failed      bool             `json:"failed"`
	Phase       string           `json:"phase"`
	LastSync    string           `json:"lastSync,omitempty"`
	Error       string           `json:"error,omitempty"`
	Conflicts   []webdavConflict `json:"conflicts"`
	Deferred    int              `json:"deferred"`
	Transferred int              `json:"transferred"`
	TotalBytes  *int64           `json:"totalBytes,omitempty"`
	Changed     []string         `json:"changed"`
	Revision    uint64           `json:"revision"`
	ApplyToken  string           `json:"applyToken,omitempty"`
}
type webdavState struct {
	accountPath string
	mu          sync.Mutex
	operation   sync.Mutex
	verified    *webdavVerification // protected by operation, never persisted
	status      webdavStatus
	next        time.Time
	cancel      context.CancelFunc
	done        chan struct{}
	closed      bool
	retryAt     time.Time
	factory     func(webdavConfig) *davClient
	beforeApply func(context.Context) error
	applyReady  chan struct{}
	applyDone   chan struct{}
	applyFailed bool
	taskID      string
}

type webdavVerification struct {
	Input    webdavConfig
	Location webdavConfig
}

func (s *Server) syncConfigPath() string { return filepath.Join(s.Store.Root, "webdav-sync.json") }
func (s *Server) readSyncConfig() (webdavConfig, error) {
	config := webdavConfig{Folder: "论文库", IntervalMinutes: 15, DeviceName: "这台设备"}
	body, err := os.ReadFile(s.syncConfigPath())
	if err != nil && !os.IsNotExist(err) {
		return config, err
	}
	if err == nil && decodeSyncJSON(body, &config) != nil {
		return config, errors.New("同步设置无法读取，请重新配置")
	}
	config.LibraryConfigured = err == nil
	config, err = s.useSyncConnection(config)
	if err != nil {
		return config, err
	}
	config.PasswordSet = config.Password != ""
	return config, nil
}
func (s *Server) syncStatePath(config webdavConfig) string {
	return filepath.Join(s.syncDirectory(), syncBytesHash([]byte(config.Account+"\x00"+config.RemoteID)), "state.json")
}
func (s *Server) loadSyncState(config webdavConfig) (*syncLocalState, error) {
	state := newSyncState()
	body, err := os.ReadFile(s.syncStatePath(config))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if decodeSyncJSON(body, state) != nil || !syncID(state.Device) || state.Clock == nil || state.Own == nil || state.Base == nil || state.BaseClocks == nil || state.Files == nil || state.Uploaded == nil || state.Remotes == nil || state.Choices == nil {
		return nil, errors.New("本地同步记录无法读取，已保留书库数据")
	}
	if file, e := os.Open(s.syncUploadPath(config)); e == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := scanner.Text()
			if hash, e := hex.DecodeString(line); e == nil && len(hash) == 32 {
				state.Uploaded[line] = true
			}
		}
		err = scanner.Err()
		file.Close()
		if err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	return state, nil
}
func (s *Server) syncUploadPath(config webdavConfig) string {
	return filepath.Join(filepath.Dir(s.syncStatePath(config)), "uploads.txt")
}
func (s *Server) recordSyncUpload(config webdavConfig, hash string) error {
	file, err := os.OpenFile(s.syncUploadPath(config), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = file.WriteString(hash + "\n")
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func (s *Server) saveSyncState(config webdavConfig, state *syncLocalState) error {
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return atomicFile(s.syncStatePath(config), body)
}
func (s *Server) dav(config webdavConfig) *davClient {
	if s.webdav.factory != nil {
		return s.webdav.factory(config)
	}
	return newDAVClient(config)
}
func (s *Server) setSyncPhase(phase string) {
	s.webdav.mu.Lock()
	s.syncStatusLocked().Phase = phase
	taskID := s.webdav.taskID
	s.webdav.mu.Unlock()
	s.diagnosticEvent(slog.LevelInfo, "sync.stage", "phase", phase, "taskId", taskID)
}
func (s *Server) syncStatusLocked() *webdavStatus { return &s.webdav.status }
func (s *Server) syncStatus() webdavStatus {
	s.webdav.mu.Lock()
	result := *s.syncStatusLocked()
	s.webdav.mu.Unlock()
	dirty, failed, err := s.syncChangeStatus()
	result.Dirty = err != nil || dirty || result.Deferred > 0 || len(result.Conflicts) > 0
	result.Failed = failed || result.Phase == "failed"
	if result.Phase == "" {
		result.Phase = "idle"
	}
	if result.Conflicts == nil {
		result.Conflicts = []webdavConflict{}
	}
	if result.Changed == nil {
		result.Changed = []string{}
	}
	return result
}

// Each device exclusively publishes its own manifest. Concurrent devices
// cannot overwrite one shared index, including on DAV servers without CAS.
func (s *Server) runWebDAV(ctx context.Context, config webdavConfig) error {
	if err := s.recoverSyncInstall(); err != nil {
		return err
	}
	var dav *davClient
	var err error
	config, dav, err = s.prepareSyncLocation(ctx, config)
	if err != nil {
		return err
	}
	state, err := s.loadSyncState(config)
	if err != nil {
		return err
	}
	s.setSyncPhase("scanning")
	snapshot, err := s.snapshotSync(ctx, state)
	if err != nil {
		return err
	}
	keys := map[string]bool{}
	for key := range state.Base {
		keys[key] = true
	}
	for key := range state.Own {
		keys[key] = true
	}
	for key := range snapshot.Documents {
		keys[key] = true
	}
	for key := range keys {
		if snapshot.Busy[strings.TrimPrefix(key, "document/")] {
			continue
		}
		local, base := snapshot.Documents[key], state.Base[key]
		own := state.Own[key]
		pending := own != nil && !state.BaseClocks[key].covers(own.Clock)
		if syncHash(local) == syncHash(base) && !pending {
			continue
		}
		if base == nil && own != nil {
			base = own.Body
		}
		if own := state.Own[key]; own != nil && (local == nil && own.Deleted || local != nil && !own.Deleted && syncHash(own.Body) == syncHash(local)) {
			continue
		}
		parents := syncClock{}
		parents.include(state.BaseClocks[key])
		if own := state.Own[key]; own != nil {
			parents.include(own.Clock)
		}
		state.Own[key] = newSyncVersion(state, local, base, config.DeviceName, parents)
	}
	// Fingerprints are a read cache, not deletion history. Keep only files in
	// this snapshot; Own/Base still retain chunks needed by deferred work.
	state.Files = snapshot.Files
	// Persist unpublished local changes before any network access. A retry
	// retains their original causal order instead of hiding an offline edit.
	if err = s.saveSyncState(config, state); err != nil {
		return err
	}
	s.setSyncPhase("downloading")
	devices, err := dav.devices(ctx)
	if err != nil {
		return err
	}
	candidates := map[string][]*syncVersion{}
	for key, entry := range state.Own {
		candidates[key] = syncCandidates(candidates[key], entry)
	}
	ownRemoteHash := ""
	for _, device := range devices {
		cache := filepath.Join(filepath.Dir(s.syncStatePath(config)), "remotes", device.Name)
		body, readErr := os.ReadFile(cache)
		if readErr != nil || device.ETag == "" || state.Remotes[device.Name].ETag != device.ETag {
			body, err = dav.get(ctx, "devices/"+device.Name, syncManifestLimit)
			if err != nil {
				return err
			}
		}
		var manifest syncManifest
		deviceID := strings.TrimSuffix(device.Name, ".json")
		if err = decodeSyncJSON(body, &manifest); err != nil {
			return err
		}
		if err = validateSyncManifest(&manifest, deviceID); err != nil {
			return err
		}
		if deviceID == state.Device {
			ownRemoteHash = syncHash(manifest)
			for key, entry := range manifest.Entries {
				if own := state.Own[key]; entry.Clock[state.Device] > state.Clock[state.Device] || own != nil && entry.Clock[state.Device] == own.Clock[state.Device] && !sameSyncValue(entry, own) {
					return errors.New("同步身份已被其他设备使用，请断开后重新连接；书库数据已保留")
				}
			}
		}
		for key, entry := range manifest.Entries {
			candidates[key] = syncCandidates(candidates[key], entry)
			state.Clock.include(entry.Clock)
			if entry.Deleted {
				continue
			}
			for _, file := range entry.Body.Files {
				for _, chunk := range file.Chunks {
					state.Uploaded[chunk.Hash] = true
				}
			}
		}
		if err = atomicFile(cache, body); err != nil {
			return err
		}
		state.Remotes[device.Name] = syncRemoteCache{ETag: device.ETag}
	}
	schema, err := syncSchema(ctx, s.Store.DB)
	if err != nil {
		return err
	}
	for key, versions := range candidates {
		for _, entry := range versions {
			if err = validateSyncDocument(key, entry.Body, schema); err != nil {
				return err
			}
		}
	}
	desired := map[string]*syncDocument{}
	resolutions := map[string]*syncVersion{}
	ackClocks := map[string]syncClock{}
	conflicts := []webdavConflict{}
	for key, versions := range candidates {
		selected := versions[0]
		merged := false
		for _, entry := range versions[1:] {
			if sameSyncValue(selected, entry) {
				continue
			}
			if !selected.Deleted && !entry.Deleted {
				if body, ok := mergeSyncDocument(state.Base[key], selected.Body, entry.Body); ok {
					selected = &syncVersion{Body: body, Clock: syncClock{}}
					merged = true
					continue
				}
			}
			selected = nil
			break
		}
		conflictID := syncHash(versions)
		if selected == nil {
			choice := state.Choices[key]
			if choice.Conflict == conflictID {
				if choice.KeepBoth {
					if body, ok := keepSyncNotes(state.Base[key], versions); ok {
						selected = &syncVersion{Body: body}
						merged = true
					}
				}
				for _, version := range versions {
					if !choice.KeepBoth && syncHash(version) == choice.Version {
						selected = version
						// Resolve just the note when all other records can merge;
						// choosing its text must not discard independent annotations.
						if body, ok := keepSyncNotes(state.Base[key], versions); ok {
							for recordKey, record := range body.Records {
								if len(record["document_notes"]) == 1 {
									if note, exists := version.Body.Records[recordKey]; exists {
										body.Records[recordKey] = note
									} else {
										delete(body.Records, recordKey)
									}
								}
							}
							selected = &syncVersion{Body: body}
						}
						merged = true
						break
					}
				}
			}
			if selected == nil {
				conflict := syncConflict(key, conflictID, versions)
				_, conflict.CanKeepBoth = keepSyncNotes(state.Base[key], versions)
				conflicts = append(conflicts, conflict)
				continue
			}
		}
		if merged {
			body := selected.Body
			if selected.Deleted {
				body = nil
			}
			selected = newSyncVersion(state, body, selected.Body, config.DeviceName)
			resolutions[key] = selected
		}
		clock := syncClock{}
		for _, version := range versions {
			clock.include(version.Clock)
		}
		clock.include(selected.Clock)
		ackClocks[key] = clock
		if selected.Deleted {
			desired[key] = nil
		} else {
			desired[key] = selected.Body
		}
	}
	slices.SortFunc(conflicts, func(a, b webdavConflict) int { return strings.Compare(a.Key, b.Key) })
	s.webdav.mu.Lock()
	s.syncStatusLocked().Conflicts = conflicts
	s.syncStatusLocked().Deferred = len(snapshot.Busy)
	s.webdav.mu.Unlock()
	if err = s.saveSyncState(config, state); err != nil {
		return err
	}
	updates := map[string]*syncDocument{}
	run := id()
	defer func() {
		if _, err := os.Stat(filepath.Join(s.syncDirectory(), "apply.json")); os.IsNotExist(err) {
			_ = os.RemoveAll(filepath.Join(s.syncDirectory(), "apply", run))
		}
	}()
	for key, body := range desired {
		if snapshot.Busy[strings.TrimPrefix(key, "document/")] {
			continue
		}
		if syncHash(body) == syncHash(snapshot.Documents[key]) {
			continue
		}
		updates[key] = body
	}
	downloadBytes, err := s.syncDownloadBytes(ctx, snapshot.Documents, updates)
	if err != nil {
		return err
	}
	uploads := maps.Clone(state.Own)
	for key, version := range resolutions {
		if !snapshot.Busy[strings.TrimPrefix(key, "document/")] {
			uploads[key] = version
		}
	}
	s.setSyncTotal(downloadBytes + syncUploadBytes(uploads, state.Uploaded))
	for key, body := range updates {
		if body != nil {
			if err = s.stageSyncDocument(ctx, dav, run, snapshot.Documents[key], body); err != nil {
				return err
			}
		}
	}
	s.setSyncPhase("applying")
	finishApply, err := s.waitSyncApply(ctx, run, len(updates) > 0)
	if err != nil {
		return err
	}
	defer finishApply()
	accepted, err := s.installSync(ctx, run, snapshot, updates)
	if err != nil {
		return err
	}
	if err = s.recoverSyncInstall(); err != nil {
		return err
	}
	s.notifySyncChanges(accepted)
	finishApply()
	for key, version := range resolutions {
		_, updating := updates[key]
		if !snapshot.Busy[strings.TrimPrefix(key, "document/")] && (!updating || accepted[key]) {
			state.Own[key] = version
		}
	}
	if err = s.saveSyncState(config, state); err != nil {
		return err
	}
	// Edits made while downloading may prevent applying a resolution. Use
	// only the versions actually accepted when counting remaining uploads.
	s.webdav.mu.Lock()
	transferred := s.webdav.status.Transferred
	s.webdav.mu.Unlock()
	s.setSyncTotal(int64(transferred) + syncUploadBytes(state.Own, state.Uploaded))
	s.setSyncPhase("uploading")
	for _, entry := range state.Own {
		if entry.Deleted {
			continue
		}
		for _, file := range entry.Body.Files {
			for _, chunk := range file.Chunks {
				if state.Uploaded[chunk.Hash] {
					continue
				}
				body, err := os.ReadFile(s.syncBlob(chunk.Hash))
				if err != nil {
					return errors.New("本地同步文件缺失，请重新扫描书库")
				}
				if len(body) != chunk.Size || syncBytesHash(body) != chunk.Hash {
					return errors.New("本地同步文件校验失败")
				}
				if err = dav.mkdir(ctx, "objects/"+chunk.Hash[:2]); err != nil {
					return err
				}
				if err = dav.put(ctx, "objects/"+chunk.Hash[:2]+"/"+chunk.Hash, body); err != nil {
					return err
				}
				state.Uploaded[chunk.Hash] = true
				if err = s.recordSyncUpload(config, chunk.Hash); err != nil {
					return err
				}
				s.webdav.mu.Lock()
				s.syncStatusLocked().Transferred += len(body)
				s.webdav.mu.Unlock()
			}
		}
	}
	manifest := syncManifest{Format: "neoread-webdav", Version: 1, Device: state.Device, Entries: state.Own}
	if syncHash(manifest) != ownRemoteHash {
		body, _ := json.Marshal(manifest)
		if len(body) > syncManifestLimit {
			return errors.New("同步元数据超过当前格式上限，请拆分为多个书库")
		}
		if err = dav.put(ctx, "devices/"+state.Device+".json", body); err != nil {
			return err
		}
	}
	for key, body := range desired {
		_, updating := updates[key]
		if updating && !accepted[key] {
			continue
		}
		if snapshot.Busy[strings.TrimPrefix(key, "document/")] {
			continue
		}
		if body == nil {
			delete(state.Base, key)
		} else {
			state.Base[key] = body
		}
		state.BaseClocks[key] = ackClocks[key]
		delete(state.Choices, key)
	}
	state.LastSync = now()
	if err = s.saveSyncState(config, state); err != nil {
		return err
	}
	_ = os.Remove(s.syncUploadPath(config))
	if len(conflicts) == 0 && len(snapshot.Busy) == 0 && len(accepted) == len(updates) {
		if err = s.confirmSyncRevision(ctx, snapshot.Revision+snapshot.AppliedChanges); err != nil {
			return err
		}
	}
	s.webdav.mu.Lock()
	s.syncStatusLocked().LastSync = state.LastSync
	s.webdav.mu.Unlock()
	if count, bytes, err := s.pruneSyncCache(ctx, config, state); err != nil {
		if ctx.Err() == nil {
			s.diagnosticEvent(slog.LevelWarn, "sync.cache_prune_failed", "taskId", s.webdav.taskID, "errorCode", "cache")
		}
	} else if count > 0 {
		s.diagnosticEvent(slog.LevelInfo, "sync.cache_pruned", "taskId", s.webdav.taskID, "count", count, "bytes", bytes)
	}
	return nil
}

func (s *Server) waitSyncApply(ctx context.Context, token string, needed bool) (func(), error) {
	if !needed {
		return func() {}, nil
	}
	if s.webdav.beforeApply != nil {
		return func() {}, s.webdav.beforeApply(ctx)
	}
	s.webdav.mu.Lock()
	s.webdav.applyReady = make(chan struct{})
	s.webdav.applyDone = make(chan struct{})
	s.webdav.applyFailed = false
	ready, done := s.webdav.applyReady, s.webdav.applyDone
	s.webdav.status.Phase = "ready"
	s.webdav.status.ApplyToken = token
	taskID := s.webdav.taskID
	s.webdav.mu.Unlock()
	s.diagnosticEvent(slog.LevelInfo, "sync.stage", "phase", "ready", "taskId", taskID)
	var once sync.Once
	finish := func() {
		once.Do(func() {
			s.webdav.mu.Lock()
			s.webdav.status.ApplyToken = ""
			s.webdav.applyReady = nil
			s.webdav.mu.Unlock()
			close(done)
		})
	}
	select {
	case <-ctx.Done():
		finish()
		return func() {}, ctx.Err()
	case <-ready:
		s.webdav.mu.Lock()
		failed := s.webdav.applyFailed
		s.webdav.mu.Unlock()
		if failed {
			finish()
			return func() {}, errors.New("阅读状态保存失败，未应用云端内容，请重试同步")
		}
		s.setSyncPhase("applying")
		return finish, nil
	}
}

func (s *Server) notifySyncChanges(changed map[string]bool) {
	if len(changed) == 0 {
		return
	}
	s.annotationMu.Lock()
	s.annotationHistory = slices.DeleteFunc(s.annotationHistory, func(change annotationChange) bool { return changed["document/"+change.document] })
	for key := range changed {
		if key != "library" {
			s.publishAnnotationChange(strings.TrimPrefix(key, "document/"))
		}
	}
	s.annotationMu.Unlock()
	s.translationMu.Lock()
	for key := range changed {
		owner := strings.TrimPrefix(key, "document/")
		for channel := range s.translationSubscribers[owner] {
			close(channel)
		}
		delete(s.translationSubscribers, owner)
	}
	s.translationMu.Unlock()
	for key := range changed {
		if key == "library" {
			continue
		}
		owner := strings.TrimPrefix(key, "document/")
		s.codexChat.deleteDocument(owner)
		s.documentCodex.deleteDocument(owner)
		if _, err := s.Store.DB.Exec("INSERT INTO document_corpus_jobs(document_id,status) SELECT id,'queued' FROM documents WHERE id=? ON CONFLICT(document_id) DO UPDATE SET status='queued'", owner); err != nil {
			s.diagnosticEvent(slog.LevelError, "sync.index_failed", "documentId", owner, "errorCode", "save")
		}
	}
	s.connectorRevision.Add(1)
	keys := []string{}
	for key := range changed {
		keys = append(keys, strings.TrimPrefix(key, "document/"))
	}
	slices.Sort(keys)
	s.webdav.mu.Lock()
	s.webdav.status.Changed = keys
	s.webdav.status.Revision++
	s.webdav.mu.Unlock()
}

func syncConflict(key, identifier string, versions []*syncVersion) webdavConflict {
	result := webdavConflict{Key: key, ID: identifier, Title: "书库分类与设置", Versions: []webdavVariant{}}
	for _, version := range versions {
		if key != "library" {
			for _, record := range version.Body.Records {
				for _, row := range record["documents"] {
					result.Title = fmtSyncString(row["title"])
				}
			}
		}
		texts := []string{}
		for _, record := range version.Body.Records {
			for _, row := range record["document_notes"] {
				texts = append(texts, fmtSyncString(row["body"]))
			}
			for _, row := range record["messages"] {
				var message struct {
					Content string `json:"content"`
				}
				_ = json.Unmarshal([]byte(fmtSyncString(row["body"])), &message)
				texts = append(texts, message.Content)
			}
		}
		preview := []rune(strings.Join(texts, "\n"))
		if len(preview) > 600 {
			preview = preview[:600]
		}
		result.Versions = append(result.Versions, webdavVariant{ID: syncHash(version), Device: version.Name, Updated: version.Updated, Deleted: version.Deleted, Preview: string(preview)})
	}
	return result
}

func (s *Server) setSyncTotal(total int64) {
	s.webdav.mu.Lock()
	s.webdav.status.TotalBytes = &total
	s.webdav.mu.Unlock()
}

func syncUploadBytes(entries map[string]*syncVersion, uploaded map[string]bool) int64 {
	var total int64
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.Deleted {
			continue
		}
		for _, file := range entry.Body.Files {
			for _, chunk := range file.Chunks {
				if !uploaded[chunk.Hash] && !seen[chunk.Hash] {
					total += int64(chunk.Size)
					seen[chunk.Hash] = true
				}
			}
		}
	}
	return total
}

func (s *Server) syncDownloadBytes(ctx context.Context, before, updates map[string]*syncDocument) (int64, error) {
	var total int64
	seen := map[string]bool{}
	for key, doc := range updates {
		if doc == nil {
			continue
		}
		for name, file := range doc.Files {
			if old := before[key]; old != nil && syncHash(old.Files[name]) == syncHash(file) {
				continue
			}
			for _, chunk := range file.Chunks {
				if err := ctx.Err(); err != nil {
					return 0, err
				}
				if seen[chunk.Hash] {
					continue
				}
				seen[chunk.Hash] = true
				body, err := os.ReadFile(s.syncBlob(chunk.Hash))
				if err != nil || len(body) != chunk.Size || syncBytesHash(body) != chunk.Hash {
					total += int64(chunk.Size)
				}
			}
		}
	}
	return total, nil
}

func (s *Server) beginWebDAVSync(ctx context.Context, manual ...bool) error {
	if !s.webdav.operation.TryLock() {
		return errors.New("同步正在进行")
	}
	config, err := s.readSyncConfig()
	if err != nil || !config.LibraryConfigured || config.Account == "" || config.Password == "" {
		s.webdav.operation.Unlock()
		return errors.New("请先保存坚果云账号和应用密码")
	}
	if !config.Enabled {
		s.webdav.operation.Unlock()
		return errors.New("请先开启此书库同步")
	}
	s.webdav.mu.Lock()
	if s.webdav.closed {
		s.webdav.mu.Unlock()
		s.webdav.operation.Unlock()
		return errors.New("应用正在关闭")
	}
	if time.Now().Before(s.webdav.retryAt) {
		s.webdav.mu.Unlock()
		s.webdav.operation.Unlock()
		return errors.New("坚果云限流等待中，请稍后重试")
	}
	child, cancel := context.WithCancel(ctx)
	s.webdav.cancel = cancel
	s.webdav.done = make(chan struct{})
	done := s.webdav.done
	s.syncStatusLocked().Phase = "scanning"
	s.syncStatusLocked().Error = ""
	s.syncStatusLocked().Transferred = 0
	s.syncStatusLocked().TotalBytes = nil
	taskID := id()
	s.webdav.taskID = taskID
	s.webdav.status.RunID = taskID
	s.webdav.status.Manual = len(manual) > 0 && manual[0]
	s.webdav.mu.Unlock()
	go func() {
		defer s.webdav.operation.Unlock()
		defer close(done)
		defer cancel()
		started := time.Now()
		s.diagnosticEvent(slog.LevelInfo, "sync.started", "taskId", taskID)
		err := s.runWebDAV(child, config)
		if err != nil && !errors.Is(err, context.Canceled) {
			if _, saveErr := s.Store.DB.Exec("INSERT INTO settings(key,value) VALUES('sync:last-result','failed') ON CONFLICT(key) DO UPDATE SET value=excluded.value"); saveErr != nil {
				s.diagnosticEvent(slog.LevelError, "sync.status_failed", "taskId", taskID, "errorCode", "save")
			}
		}
		phase := "complete"
		level := slog.LevelInfo
		code := ""
		s.webdav.mu.Lock()
		if err != nil {
			phase = "failed"
			level = slog.LevelError
			code = "sync_failed"
			s.syncStatusLocked().Error = err.Error()
			var remote *davError
			if errors.As(err, &remote) && (remote.Status == 429 || remote.Status == 503) {
				s.webdav.retryAt = time.Now().Add(remote.Retry)
				code = "rate_limited"
			}
			if errors.Is(err, context.Canceled) {
				phase = "cancelled"
				level = slog.LevelInfo
				code = "cancelled"
				s.syncStatusLocked().Error = ""
			}
		} else if len(s.syncStatusLocked().Conflicts) > 0 {
			phase = "conflict"
		}
		s.syncStatusLocked().Phase = phase
		s.webdav.next = time.Now().Add(time.Duration(config.IntervalMinutes) * time.Minute)
		if err == nil {
			if _, e := os.Stat(s.syncNamePath()); e == nil {
				s.webdav.next = time.Time{}
			}
		}
		s.webdav.cancel = nil
		s.webdav.mu.Unlock()
		s.diagnosticEvent(level, "sync.finished", "taskId", taskID, "phase", phase, "errorCode", code, "durationMs", time.Since(started).Milliseconds())
	}()
	return nil
}

func (s *Server) StartWebDAV(ctx context.Context) {
	if err := s.recoverSyncInstall(); err != nil {
		s.webdav.mu.Lock()
		s.webdav.status.Phase = "failed"
		s.webdav.status.Error = "同步恢复未完成，请检查书库目录权限"
		s.webdav.mu.Unlock()
		return
	}
	if config, err := s.readSyncConfig(); err == nil && config.RemoteID != "" {
		if state, err := s.loadSyncState(config); err == nil {
			s.webdav.mu.Lock()
			s.webdav.status.LastSync = state.LastSync
			if last, err := time.Parse(time.RFC3339Nano, state.LastSync); err == nil {
				s.webdav.next = last.Add(time.Duration(config.IntervalMinutes) * time.Minute)
			}
			if _, err := os.Stat(s.syncNamePath()); err == nil {
				s.webdav.next = time.Time{}
			}
			s.webdav.mu.Unlock()
		}
	}
	go func() {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		startup := true
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			config, err := s.readSyncConfig()
			s.webdav.mu.Lock()
			next, closed := s.webdav.next, s.webdav.closed
			s.webdav.mu.Unlock()
			if closed {
				return
			}
			if err == nil && config.Enabled && config.Account != "" && config.Password != "" && (startup || !time.Now().Before(next)) {
				_ = s.beginWebDAVSync(ctx)
			}
			startup = false
			timer.Reset(30 * time.Second)
		}
	}()
}
func (s *Server) stopWebDAV() {
	s.webdav.mu.Lock()
	s.webdav.closed = true
	if s.webdav.cancel != nil {
		s.webdav.cancel()
	}
	done := s.webdav.done
	s.webdav.mu.Unlock()
	if done != nil {
		<-done
	}
}
