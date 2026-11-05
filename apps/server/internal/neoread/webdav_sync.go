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
	RunID         string           `json:"runId,omitempty"`
	Manual        bool             `json:"manual"`
	Dirty         bool             `json:"dirty"`
	Failed        bool             `json:"failed"`
	Phase         string           `json:"phase"`
	LastSync      string           `json:"lastSync,omitempty"`
	Error         string           `json:"error,omitempty"`
	Conflicts     []webdavConflict `json:"conflicts"`
	Deferred      int              `json:"deferred"`
	Transferred   int              `json:"transferred"`
	TotalBytes    *int64           `json:"totalBytes,omitempty"`
	Changed       []string         `json:"changed"`
	Revision      uint64           `json:"revision"`
	ApplyToken    string           `json:"applyToken,omitempty"`
	DocumentID    string           `json:"documentId,omitempty"`
	DocumentPhase string           `json:"documentPhase,omitempty"`
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
	state.Connection = filepath.Base(filepath.Dir(s.syncStatePath(config)))
	root, err := os.OpenRoot(s.Store.Root)
	if err != nil {
		return nil, err
	}
	name, err := filepath.Rel(s.Store.Root, s.syncStatePath(config))
	if err != nil {
		root.Close()
		return nil, err
	}
	stored, err := readSyncStoredState(root, name)
	root.Close()
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	state = stored
	state.Connection = filepath.Base(filepath.Dir(s.syncStatePath(config)))
	if !syncID(state.Device) || state.Clock == nil || state.Own == nil || state.Base == nil || state.BaseClocks == nil || state.Files == nil || state.Uploaded == nil || state.Remotes == nil || state.Choices == nil {
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
	return s.writeSyncStoredState(config, state)
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
	config, dav, err := s.prepareSyncLocation(ctx, config)
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
	snapshot.State, snapshot.DeviceName = state, config.DeviceName
	snapshot.Receipts = map[string]*syncCommitReceipt{}
	state.Files = snapshot.Files
	if err = s.saveSyncState(config, state); err != nil {
		return err
	}

	s.setSyncPhase("downloading")
	devices, err := dav.devices(ctx)
	if davNotFound(err) {
		if err = dav.prepare(ctx); err != nil {
			return err
		}
		if err = ensureSyncLocationMarker(ctx, dav, syncLocation{ID: config.RemoteID, Folder: config.Folder}); err != nil {
			return err
		}
		devices, err = dav.devices(ctx)
	}
	if err != nil {
		return err
	}
	catalogs := []*syncDeviceCatalog{}
	ownCatalog := newSyncDeviceCatalog(state.Device, config.DeviceName)
	incoming := map[string]bool{}
	deviceNames := map[string]string{}
	for _, device := range devices {
		cache := filepath.Join(filepath.Dir(s.syncStatePath(config)), "remotes", device.Name)
		previous, readErr := os.ReadFile(cache)
		body := previous
		if readErr != nil || device.ETag == "" || state.Remotes[device.Name].ETag != device.ETag {
			body, err = dav.get(ctx, "devices/"+device.Name, syncManifestLimit)
			if err != nil {
				return err
			}
		}
		deviceID := strings.TrimSuffix(device.Name, ".json")
		catalog, err := decodeSyncDeviceHeader(body, deviceID)
		if err != nil {
			return err
		}
		if deviceID == state.Device {
			if catalog.Index.Clock > state.Clock[state.Device] {
				return errors.New("同步身份已被其他设备使用，请断开后重新连接；书库数据已保留")
			}
			ownCatalog = catalog
		}
		deviceNames[deviceID] = catalog.Index.Name
		for _, key := range catalog.keys() {
			incoming[key] = true
		}
		catalogs = append(catalogs, catalog)
		if err = atomicFile(cache, []byte(jsonText(catalog.Index))); err != nil {
			return err
		}
		state.Remotes[device.Name] = syncRemoteCache{ETag: device.ETag}
	}
	present := map[string]bool{}
	for _, device := range devices {
		present[device.Name] = true
	}
	_, ownCacheErr := os.Stat(filepath.Join(filepath.Dir(s.syncStatePath(config)), "remotes", state.Device+".json"))
	if !present[state.Device+".json"] && (state.LastSync != "" || ownCacheErr == nil) {
		// The directory may have been cleared outside NeoRead. Upload receipts
		// from the previous cloud contents cannot prove that chunks still exist.
		state.Uploaded = map[string]bool{}
		if err = os.Remove(s.syncUploadPath(config)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	remotesDirectory := filepath.Join(filepath.Dir(s.syncStatePath(config)), "remotes")
	cached, err := os.ReadDir(remotesDirectory)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, remote := range cached {
		device := strings.TrimSuffix(remote.Name(), ".json")
		if strings.HasSuffix(remote.Name(), ".json") && syncID(device) && !present[remote.Name()] {
			if err = os.Remove(filepath.Join(remotesDirectory, remote.Name())); err != nil {
				return err
			}
			delete(state.Remotes, remote.Name())
		}
	}
	ownCatalog.Index.Name = config.DeviceName
	priorOwn := maps.Clone(state.Own)
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
	for key := range incoming {
		keys[key] = true
	}
	for _, receiving := range snapshot.Receiving {
		if receiving.Connection == state.Connection {
			for _, file := range receiving.Remote.Files {
				for _, chunk := range file.Chunks {
					state.Uploaded[chunk.Hash] = true
				}
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		if snapshot.Busy[strings.TrimPrefix(key, "document/")] {
			continue
		}
		local, base := snapshot.Documents[key], state.Base[key]
		if receiving := snapshot.Receiving[key]; receiving != nil && syncHash(snapshot.observed(key)) == syncHash(receiving.Baseline) {
			continue
		}
		own := state.Own[key]
		pending := own != nil && !state.BaseClocks[key].covers(own.Clock)
		if syncHash(local) == syncHash(base) && !pending {
			continue
		}
		if base == nil && own != nil {
			base = own.Body
		}
		if own != nil && (local == nil && own.Deleted || local != nil && !own.Deleted && syncHash(own.Body) == syncHash(local)) {
			continue
		}
		parents := syncClock{}
		parents.include(state.BaseClocks[key])
		if own != nil {
			parents.include(own.Clock)
		}
		state.Own[key] = newSyncVersion(state, local, base, config.DeviceName, parents)
	}
	if err = s.saveSyncState(config, state); err != nil {
		return err
	}
	schema, err := syncSchema(ctx, s.Store.DB)
	if err != nil {
		return err
	}
	ordered := slices.Sorted(maps.Keys(keys))
	slices.SortFunc(ordered, func(a, b string) int {
		if a == "library" {
			return -1
		}
		if b == "library" {
			return 1
		}
		return strings.Compare(a, b)
	})
	desired := map[string]*syncDocument{}
	accepted := map[string]bool{}
	required := map[string]bool{}
	ordinary := map[string]*syncDocument{}
	finishes := map[string]syncPDFReceiveFinish{}
	conflicts := []webdavConflict{}
	for position, key := range ordered {
		if err = ctx.Err(); err != nil {
			return err
		}
		versions := []*syncVersion{}
		if own := state.Own[key]; own != nil {
			versions = syncCandidates(versions, own)
		}
		for _, catalog := range catalogs {
			version, e := catalog.version(key, s.metadataReader(ctx, dav, config))
			if e != nil {
				return e
			}
			if version == nil {
				continue
			}
			if version.Device == state.Device {
				if previous := priorOwn[key]; previous != nil && version.Clock[state.Device] == previous.Clock[state.Device] && !sameSyncValue(version, previous) {
					return errors.New("同步身份存在不同内容，请断开后重新连接；书库数据已保留")
				}
			}
			if e = validateSyncDocument(key, version.Body, schema); e != nil {
				return e
			}
			versions = syncCandidates(versions, version)
			state.Clock.include(version.Clock)
			if !version.Deleted {
				for _, file := range version.Body.Files {
					for _, chunk := range file.Chunks {
						state.Uploaded[chunk.Hash] = true
					}
				}
			}
		}
		if len(versions) == 0 {
			continue
		}
		for _, version := range versions {
			if err = validateSyncDocument(key, version.Body, schema); err != nil {
				return err
			}
		}
		selected, merged, conflict := resolveSyncVersions(state, key, versions, config.DeviceName)
		if conflict != nil {
			for i, version := range versions {
				if name := deviceNames[version.Device]; name != "" {
					conflict.Versions[i].Device = name
				}
			}
			conflicts = append(conflicts, *conflict)
			if snapshot.observed(key) == nil && !snapshot.Busy[strings.TrimPrefix(key, "document/")] {
				if original := originalSyncVersion(versions); original != nil {
					if _, err = s.receiveSyncPDF(ctx, dav, config, snapshot, state, key, original.Body, original.Body, original.Clock, 0); err != nil {
						return err
					}
				}
			}
			continue
		}
		clock := syncClock{}
		for _, version := range versions {
			clock.include(version.Clock)
		}
		clock.include(selected.Clock)
		body := selected.Body
		if selected.Deleted {
			body = nil
		}
		desired[key] = body
		if snapshot.Busy[strings.TrimPrefix(key, "document/")] {
			continue
		}
		if syncHash(body) == syncHash(snapshot.observed(key)) && snapshot.Receiving[key] == nil {
			state.Base[key], state.BaseClocks[key] = body, clock
			if merged {
				state.Own[key] = selected
			}
			delete(state.Choices, key)
			continue
		}
		required[key] = true
		receipt := &syncCommitReceipt{Connection: state.Connection, Base: body, Clock: maps.Clone(clock)}
		if merged {
			receipt.Own = selected
		}
		snapshot.Receipts[key] = receipt
		if position == len(ordered)-1 {
			remaining := maps.Clone(ordinary)
			remaining[key] = body
			bytes, e := s.syncDownloadBytes(ctx, snapshot.Observed, remaining)
			if e != nil {
				return e
			}
			s.webdav.mu.Lock()
			transferred := s.webdav.status.Transferred
			s.webdav.mu.Unlock()
			s.setSyncTotal(int64(transferred) + bytes + syncUploadBytes(state.Own, state.Uploaded))
		}
		if body != nil && syncPDF(body) && (snapshot.observed(key) == nil || snapshot.Receiving[key] != nil) {
			finish := syncPDFReceiveFinish{Starting: copySyncDocument(snapshot.observed(key)), Desired: body}
			if previous := snapshot.Receiving[key]; previous != nil {
				finish.Known = previous.Baseline
			}
			source, sourceClock := receivingSource(snapshot, key, versions, state.Device, body)
			complete, e := s.receiveSyncPDF(ctx, dav, config, snapshot, state, key, body, source, sourceClock, 2)
			if e != nil {
				return e
			}
			if complete {
				accepted[key] = true
			} else {
				finishes[key] = finish
			}
		} else {
			ordinary[key] = body
		}
	}
	slices.SortFunc(conflicts, func(a, b webdavConflict) int { return strings.Compare(a.Key, b.Key) })
	s.webdav.mu.Lock()
	s.webdav.status.Conflicts = conflicts
	s.webdav.status.Deferred = len(snapshot.Busy)
	s.webdav.mu.Unlock()
	if err = s.saveSyncState(config, state); err != nil {
		return err
	}
	// Existing documents and deletions commit after new originals, preserving
	// shared attachments and references to documents received in this run.
	for _, key := range slices.Sorted(maps.Keys(ordinary)) {
		s.setSyncPhase("downloading")
		if key != "library" {
			s.setSyncDocumentPhase(key, "other")
		}
		ok, e := s.applySyncTarget(ctx, dav, snapshot, key, copySyncDocument(ordinary[key]), nil)
		if e != nil {
			return e
		}
		if ok {
			accepted[key] = true
		}
		state.Files = snapshot.Files
		if e = s.saveSyncDocumentState(config, state, key); e != nil {
			return e
		}
	}
	for _, key := range slices.Sorted(maps.Keys(finishes)) {
		ok, e := s.finishReceivedPDF(ctx, dav, snapshot, key, finishes[key], desired, incoming)
		if e != nil {
			return e
		}
		if ok {
			accepted[key] = true
		}
		state.Files = snapshot.Files
		if e = s.saveSyncDocumentState(config, state, key); e != nil {
			return e
		}
	}
	s.webdav.mu.Lock()
	transferred := s.webdav.status.Transferred
	s.webdav.mu.Unlock()
	s.setSyncTotal(int64(transferred) + syncUploadBytes(state.Own, state.Uploaded))
	s.setSyncPhase("uploading")
	uploadKeys := slices.Collect(maps.Keys(state.Own))
	slices.SortFunc(uploadKeys, func(a, b string) int {
		x, y := state.Own[a].Clock[state.Device], state.Own[b].Clock[state.Device]
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
		return strings.Compare(a, b)
	})
	for _, key := range uploadKeys {
		if snapshot.Receiving[key] != nil {
			continue
		}
		entry := state.Own[key]
		if key != "library" {
			s.setSyncDocumentPhase(key, "original")
		}
		if !entry.Deleted {
			for _, name := range syncFileOrder(entry.Body.Files) {
				if key != "library" {
					s.setSyncDocumentPhase(key, syncPartNames[syncFilePhase(name)])
				}
				for _, chunk := range entry.Body.Files[name].Chunks {
					if state.Uploaded[chunk.Hash] {
						continue
					}
					body, e := os.ReadFile(s.syncBlob(chunk.Hash))
					if e != nil {
						return errors.New("本地同步文件缺失，请重新扫描书库")
					}
					if len(body) != chunk.Size || syncBytesHash(body) != chunk.Hash {
						return errors.New("本地同步文件校验失败")
					}
					if e = dav.mkdir(ctx, "objects/"+chunk.Hash[:2]); e != nil {
						return e
					}
					if e = dav.put(ctx, "objects/"+chunk.Hash[:2]+"/"+chunk.Hash, body); e != nil {
						return e
					}
					state.Uploaded[chunk.Hash] = true
					if e = s.recordSyncUpload(config, chunk.Hash); e != nil {
						return e
					}
					s.webdav.mu.Lock()
					s.webdav.status.Transferred += len(body)
					s.webdav.mu.Unlock()
				}
			}
		}
		if err = s.publishSyncVersion(ctx, dav, config, ownCatalog, key, entry); err != nil {
			return err
		}
	}
	if err = s.publishSyncIndex(ctx, dav, config, ownCatalog); err != nil {
		return err
	}
	state.LastSync = now()
	if err = s.saveSyncState(config, state); err != nil {
		return err
	}
	_ = os.Remove(s.syncUploadPath(config))
	if len(conflicts) == 0 && len(snapshot.Busy) == 0 && len(snapshot.Receiving) == 0 && len(accepted) == len(required) {
		final, e := s.snapshotSync(ctx, state)
		if e != nil {
			return e
		}
		same := true
		all := map[string]bool{}
		for key := range final.Documents {
			all[key] = true
		}
		for key := range state.Base {
			all[key] = true
		}
		for key := range all {
			if syncHash(final.Documents[key]) != syncHash(state.Base[key]) {
				same = false
				break
			}
		}
		if same && len(final.Busy) == 0 {
			if e = s.confirmSyncRevision(ctx, final.Revision); e != nil {
				return e
			}
		}
	}
	s.webdav.mu.Lock()
	s.webdav.status.LastSync = state.LastSync
	s.webdav.status.DocumentID, s.webdav.status.DocumentPhase = "", ""
	s.webdav.status.Deferred += len(snapshot.Receiving)
	s.webdav.mu.Unlock()
	if count, bytes, e := s.pruneSyncCache(ctx, config, state); e != nil {
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
	s.webdav.status.Changed = slices.Compact(slices.Sorted(slices.Values(append(s.webdav.status.Changed, keys...))))
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
	s.syncStatusLocked().Changed = nil
	s.syncStatusLocked().DocumentID = ""
	s.syncStatusLocked().DocumentPhase = ""
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
