package neoread

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var syncPartNames = []string{"original", "translation", "other"}

// A device publishes only a compact index. Document versions and their three
// parts are immutable, so another reader never observes mixed versions while
// an index is being replaced. File chunks keep the existing deduplication.
type syncDeviceIndex struct {
	Format  string            `json:"format"`
	Version int               `json:"version"`
	Device  string            `json:"device"`
	Clock   uint64            `json:"clock"`
	Name    string            `json:"name,omitempty"`
	Legacy  string            `json:"legacy,omitempty"`
	Entries map[string]string `json:"entries"`
}

type syncDocumentHead struct {
	Format  string            `json:"format"`
	Version int               `json:"version"`
	Key     string            `json:"key"`
	Clock   syncClock         `json:"clock"`
	Deleted bool              `json:"deleted,omitempty"`
	Device  string            `json:"device"`
	Name    string            `json:"name"`
	Updated string            `json:"updated"`
	Parts   map[string]string `json:"parts"`
}

type syncDeviceCatalog struct {
	Index    syncDeviceIndex
	Versions map[string]*syncVersion
	Known    map[string]bool
	Hash     string
	Legacy   *syncManifest
}

func decodeSyncDeviceHeader(body []byte, device string) (*syncDeviceCatalog, error) {
	var index syncDeviceIndex
	if decodeSyncJSON(body, &index) != nil || index.Format != "neoread-webdav" || index.Version != 2 || !syncID(device) || index.Device != device || index.Entries == nil || utf8.RuneCountInString(index.Name) > 80 || strings.ContainsFunc(index.Name, unicode.IsControl) || index.Legacy != "" && !syncCacheHash(index.Legacy) {
		return nil, errors.New("云端同步索引与当前版本不兼容")
	}
	for key, hash := range index.Entries {
		if _, err := syncMetadataPath(key, "versions", hash); err != nil {
			return nil, err
		}
	}
	return &syncDeviceCatalog{Index: index, Versions: map[string]*syncVersion{}, Known: map[string]bool{}, Hash: syncBytesHash(body)}, nil
}

func syncMetadataDirectory(key string) (string, error) {
	if key == "library" {
		return "library", nil
	}
	if !strings.HasPrefix(key, "document/") || !safeSyncID(strings.TrimPrefix(key, "document/")) {
		return "", errors.New("同步文档标识无效")
	}
	return "documents/" + strings.TrimPrefix(key, "document/"), nil
}

func syncMetadataPath(key, section, hash string) (string, error) {
	directory, err := syncMetadataDirectory(key)
	if err != nil || !syncCacheHash(hash) || !slices.Contains(append([]string{"versions"}, syncPartNames...), section) {
		return "", errors.New("同步元数据引用无效")
	}
	return path.Join(directory, section, hash+".json"), nil
}

func splitSyncDocument(body *syncDocument) map[string]*syncDocument {
	parts := map[string]*syncDocument{}
	for _, name := range syncPartNames {
		parts[name] = emptySyncDocument()
	}
	for key, record := range body.Records {
		parts[syncPartNames[syncRecordPhase(record)]].Records[key] = record
	}
	for name, file := range body.Files {
		parts[syncPartNames[syncFilePhase(name)]].Files[name] = file
	}
	return parts
}

type syncMetadataReader func(string, string) ([]byte, error)

func decodeSyncDeviceIndex(body []byte, device string, read syncMetadataReader) (*syncDeviceCatalog, error) {
	catalog, err := decodeSyncDeviceHeader(body, device)
	if err != nil {
		return nil, err
	}
	for key, hash := range catalog.Index.Entries {
		name, err := syncMetadataPath(key, "versions", hash)
		if err != nil {
			return nil, err
		}
		body, err := read(name, hash)
		if err != nil {
			return nil, err
		}
		var head syncDocumentHead
		if decodeSyncJSON(body, &head) != nil || head.Format != "neoread-document" || head.Version != 2 || head.Key != key || head.Device != device || len(head.Parts) != len(syncPartNames) {
			return nil, errors.New("云端论文版本无效")
		}
		catalog.Known[name] = true
		document := emptySyncDocument()
		for phase, partName := range syncPartNames {
			partHash := head.Parts[partName]
			file, err := syncMetadataPath(key, partName, partHash)
			if err != nil {
				return nil, err
			}
			body, err := read(file, partHash)
			if err != nil {
				return nil, err
			}
			var part syncDocument
			if decodeSyncJSON(body, &part) != nil || part.Records == nil || part.Files == nil {
				return nil, errors.New("云端论文内容不完整")
			}
			for recordKey, record := range part.Records {
				if syncRecordPhase(record) != phase {
					return nil, errors.New("云端论文记录阶段不一致")
				}
				document.Records[recordKey] = record
			}
			for filename, contents := range part.Files {
				if syncFilePhase(filename) != phase {
					return nil, errors.New("云端论文文件阶段不一致")
				}
				document.Files[filename] = contents
			}
			catalog.Known[file] = true
		}
		if head.Clock[device] > catalog.Index.Clock {
			return nil, errors.New("云端论文版本超出设备索引")
		}
		catalog.Versions[key] = &syncVersion{Clock: head.Clock, Deleted: head.Deleted, Body: document, Device: head.Device, Name: head.Name, Updated: head.Updated}
	}
	if catalog.Index.Legacy != "" {
		if err := catalog.loadLegacy(read); err != nil {
			return nil, err
		}
		for key, version := range catalog.Legacy.Entries {
			selected, err := newerSyncDeviceVersion(catalog.Versions[key], version, device)
			if err != nil {
				return nil, err
			}
			catalog.Versions[key] = selected
		}
	}
	manifest := syncManifest{Format: "neoread-webdav", Version: 1, Device: device, Entries: catalog.Versions}
	if err := validateSyncManifest(&manifest, device); err != nil {
		return nil, err
	}
	return catalog, nil
}

func (s *Server) metadataReader(ctx context.Context, dav *davClient, config webdavConfig) syncMetadataReader {
	return func(name, hash string) ([]byte, error) {
		cache := filepath.Join(filepath.Dir(s.syncStatePath(config)), "metadata", filepath.FromSlash(name))
		body, err := os.ReadFile(cache)
		if err == nil && syncBytesHash(body) == hash {
			return body, nil
		}
		body, err = dav.get(ctx, name, syncManifestLimit)
		if err != nil {
			return nil, err
		}
		if syncBytesHash(body) != hash {
			return nil, errors.New("云端论文元数据校验失败")
		}
		if err = atomicFile(cache, body); err != nil {
			return nil, err
		}
		return body, nil
	}
}

func mkdirSyncMetadata(ctx context.Context, dav *davClient, directory string) error {
	current := ""
	for _, part := range strings.Split(directory, "/") {
		current = path.Join(current, part)
		if err := dav.mkdir(ctx, current); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) putSyncMetadata(ctx context.Context, dav *davClient, config webdavConfig, catalog *syncDeviceCatalog, key, section string, value any) (string, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if len(body) > syncManifestLimit {
		return "", errors.New("单篇论文同步元数据超过格式上限")
	}
	hash := syncBytesHash(body)
	name, err := syncMetadataPath(key, section, hash)
	if err != nil {
		return "", err
	}
	if catalog.Known[name] {
		return hash, nil
	}
	if err = mkdirSyncMetadata(ctx, dav, path.Dir(name)); err != nil {
		return "", err
	}
	if err = dav.put(ctx, name, body); err != nil {
		return "", err
	}
	cache := filepath.Join(filepath.Dir(s.syncStatePath(config)), "metadata", filepath.FromSlash(name))
	if err = atomicFile(cache, body); err != nil {
		return "", err
	}
	catalog.Known[name] = true
	return hash, nil
}

func (s *Server) publishSyncVersion(ctx context.Context, dav *davClient, config webdavConfig, catalog *syncDeviceCatalog, key string, version *syncVersion) error {
	head := syncDocumentHead{Format: "neoread-document", Version: 2, Key: key, Clock: version.Clock, Deleted: version.Deleted, Device: version.Device, Name: version.Name, Updated: version.Updated, Parts: map[string]string{}}
	parts := splitSyncDocument(version.Body)
	for _, section := range syncPartNames {
		head.Parts[section] = syncHash(parts[section])
	}
	if catalog.Index.Entries[key] == syncHash(head) {
		return nil
	}
	for _, section := range syncPartNames {
		hash, err := s.putSyncMetadata(ctx, dav, config, catalog, key, section, parts[section])
		if err != nil {
			return err
		}
		head.Parts[section] = hash
	}
	hash, err := s.putSyncMetadata(ctx, dav, config, catalog, key, "versions", head)
	if err != nil {
		return err
	}
	catalog.Index.Entries[key] = hash
	if version.Clock[catalog.Index.Device] > catalog.Index.Clock {
		catalog.Index.Clock = version.Clock[catalog.Index.Device]
	}
	catalog.Versions[key] = version
	return s.publishSyncIndex(ctx, dav, config, catalog)
}

func (s *Server) publishSyncIndex(ctx context.Context, dav *davClient, config webdavConfig, catalog *syncDeviceCatalog) error {
	body, err := json.Marshal(catalog.Index)
	if err != nil {
		return err
	}
	if len(body) > syncManifestLimit {
		return errors.New("同步索引超过格式上限，请拆分书库")
	}
	hash := syncBytesHash(body)
	if hash == catalog.Hash {
		return nil
	}
	name := "devices/" + catalog.Index.Device + ".json"
	if err = dav.put(ctx, name, body); err != nil {
		return err
	}
	if err = atomicFile(filepath.Join(filepath.Dir(s.syncStatePath(config)), "remotes", catalog.Index.Device+".json"), body); err != nil {
		return err
	}
	catalog.Hash = hash
	return nil
}

func newSyncDeviceCatalog(device, name string) *syncDeviceCatalog {
	return &syncDeviceCatalog{Index: syncDeviceIndex{Format: "neoread-webdav", Version: 2, Device: device, Name: name, Entries: map[string]string{}}, Versions: map[string]*syncVersion{}, Known: map[string]bool{}}
}

func legacySyncMetadataPath(device, hash string) string {
	return path.Join(".legacy", "devices", device, hash+".json")
}

func (catalog *syncDeviceCatalog) loadLegacy(read syncMetadataReader) error {
	if catalog.Legacy != nil || catalog.Index.Legacy == "" {
		return nil
	}
	name := legacySyncMetadataPath(catalog.Index.Device, catalog.Index.Legacy)
	body, err := read(name, catalog.Index.Legacy)
	if err != nil {
		return err
	}
	var legacy syncManifest
	if decodeSyncJSON(body, &legacy) != nil {
		return errors.New("旧版同步备份无法读取")
	}
	if err = validateSyncManifest(&legacy, catalog.Index.Device); err != nil {
		return err
	}
	catalog.Legacy = &legacy
	catalog.Known[name] = true
	return nil
}

func newerSyncDeviceVersion(a, b *syncVersion, device string) (*syncVersion, error) {
	if a == nil {
		return b, nil
	}
	if b == nil {
		return a, nil
	}
	if a.Clock[device] == b.Clock[device] && !sameSyncValue(a, b) {
		return nil, errors.New("旧版设备覆盖了不同的同步版本，已保留双方数据")
	}
	if a.Clock.covers(b.Clock) {
		return a, nil
	}
	if b.Clock.covers(a.Clock) {
		return b, nil
	}
	return nil, errors.New("设备的同步身份不一致，已保留双方数据")
}

func (catalog *syncDeviceCatalog) keys() []string {
	keys := map[string]bool{}
	for key := range catalog.Index.Entries {
		keys[key] = true
	}
	if catalog.Legacy != nil {
		for key := range catalog.Legacy.Entries {
			keys[key] = true
		}
	}
	return slices.Sorted(maps.Keys(keys))
}

func (catalog *syncDeviceCatalog) version(key string, read syncMetadataReader) (*syncVersion, error) {
	if version := catalog.Versions[key]; version != nil {
		return version, nil
	}
	var version *syncVersion
	if hash := catalog.Index.Entries[key]; hash != "" {
		index := catalog.Index
		index.Legacy = ""
		index.Entries = map[string]string{key: hash}
		decoded, err := decodeSyncDeviceIndex([]byte(jsonText(index)), index.Device, read)
		if err != nil {
			return nil, err
		}
		version = decoded.Versions[key]
		for file := range decoded.Known {
			catalog.Known[file] = true
		}
	}
	if catalog.Legacy != nil {
		var err error
		version, err = newerSyncDeviceVersion(version, catalog.Legacy.Entries[key], catalog.Index.Device)
		if err != nil {
			return nil, err
		}
	}
	if version != nil {
		catalog.Versions[key] = version
	}
	return version, nil
}

func (s *Server) readSyncDevice(ctx context.Context, dav *davClient, config webdavConfig, device string, body, previous []byte) (*syncDeviceCatalog, error) {
	var header struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(body, &header) != nil {
		return nil, errors.New("云端同步清单无效")
	}
	read := s.metadataReader(ctx, dav, config)
	if header.Version == 2 {
		catalog, err := decodeSyncDeviceHeader(body, device)
		if err == nil {
			err = catalog.loadLegacy(read)
		}
		return catalog, err
	}
	var legacy syncManifest
	if decodeSyncJSON(body, &legacy) != nil {
		return nil, errors.New("云端同步清单无法读取")
	}
	if err := validateSyncManifest(&legacy, device); err != nil {
		return nil, err
	}
	s.diagnosticEvent(slog.LevelInfo, "sync.storage_migration_started", "taskId", s.webdav.taskID, "count", len(legacy.Entries), "phase", "cloud")
	catalog := newSyncDeviceCatalog(device, "")
	var oldHeader struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(previous, &oldHeader) == nil && oldHeader.Version == 2 {
		old, err := decodeSyncDeviceHeader(previous, device)
		if err != nil {
			return nil, err
		}
		catalog.Index.Entries = maps.Clone(old.Index.Entries)
		catalog.Index.Name, catalog.Index.Clock = old.Index.Name, old.Index.Clock
	}
	latest := ""
	needName := catalog.Index.Name == ""
	for _, version := range legacy.Entries {
		if version.Clock[device] > catalog.Index.Clock {
			catalog.Index.Clock = version.Clock[device]
		}
		if needName && (latest == "" || syncTimeAfter(version.Updated, latest)) && version.Name != "" {
			catalog.Index.Name, latest = version.Name, version.Updated
		}
	}
	catalog.Index.Legacy = syncBytesHash(body)
	backup := legacySyncMetadataPath(device, catalog.Index.Legacy)
	if err := mkdirSyncMetadata(ctx, dav, path.Dir(backup)); err != nil {
		return nil, err
	}
	if err := dav.put(ctx, backup, body); err != nil {
		return nil, err
	}
	cache := filepath.Join(filepath.Dir(s.syncStatePath(config)), "metadata", filepath.FromSlash(backup))
	if err := atomicFile(cache, body); err != nil {
		return nil, err
	}
	catalog.Legacy = &legacy
	catalog.Known[backup] = true
	// The legacy backup keeps every unconverted document reachable. Publishing
	// this small versioned index also fences older clients before file installs.
	if err := s.publishSyncIndex(ctx, dav, config, catalog); err != nil {
		return nil, err
	}
	return catalog, nil
}

func (s *Server) finishSyncStorageMigration(ctx context.Context, dav *davClient, config webdavConfig, catalog *syncDeviceCatalog) error {
	if catalog.Index.Legacy == "" {
		return nil
	}
	for _, key := range catalog.keys() {
		version, err := catalog.version(key, s.metadataReader(ctx, dav, config))
		if err != nil {
			return err
		}
		if err = s.publishSyncVersion(ctx, dav, config, catalog, key, version); err != nil {
			return err
		}
	}
	catalog.Index.Legacy = ""
	if err := s.publishSyncIndex(ctx, dav, config, catalog); err != nil {
		return err
	}
	s.diagnosticEvent(slog.LevelInfo, "sync.storage_migrated", "taskId", s.webdav.taskID, "count", len(catalog.Index.Entries), "phase", "cloud")
	catalog.Legacy = nil
	return nil
}

func readSyncCachedManifest(root *os.Root, name, device string) (*syncManifest, error) {
	body, err := root.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var header struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(body, &header) != nil {
		return nil, errors.New("同步缓存清单无效")
	}
	if header.Version != 2 {
		var manifest syncManifest
		if err = readSyncCacheJSON(root, name, &manifest); err != nil {
			return nil, err
		}
		return &manifest, validateSyncManifest(&manifest, device)
	}
	read := func(remote, hash string) ([]byte, error) {
		file := filepath.Join(filepath.Dir(filepath.Dir(name)), "metadata", filepath.FromSlash(remote))
		info, err := root.Lstat(file)
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("同步元数据缓存缺失")
		}
		body, err := root.ReadFile(file)
		if err != nil || syncBytesHash(body) != hash {
			return nil, errors.New("同步元数据缓存校验失败")
		}
		return body, nil
	}
	catalog, err := decodeSyncDeviceIndex(body, device, read)
	if err != nil {
		return nil, err
	}
	return &syncManifest{Format: "neoread-webdav", Version: 1, Device: device, Entries: catalog.Versions}, nil
}
