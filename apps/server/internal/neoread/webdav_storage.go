package neoread

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const syncStorageVersion = 3

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
}

func decodeSyncDeviceHeader(body []byte, device string) (*syncDeviceCatalog, error) {
	return decodeSyncDeviceHeaderVersion(body, device, syncStorageVersion)
}

// Version 2 is accepted only for local caches left by an earlier installation.
func decodeSyncDeviceHeaderVersion(body []byte, device string, version int) (*syncDeviceCatalog, error) {
	var index syncDeviceIndex
	if decodeSyncJSON(body, &index) != nil || index.Format != "neoread-webdav" || index.Version != version || !syncID(device) || index.Device != device || index.Entries == nil || utf8.RuneCountInString(index.Name) > 80 || strings.ContainsFunc(index.Name, unicode.IsControl) {
		return nil, errors.New("云端同步索引与当前版本不兼容")
	}
	for key, hash := range index.Entries {
		if _, err := syncMetadataPathVersion(key, "versions", hash, version); err != nil {
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
	return syncMetadataPathVersion(key, section, hash, syncStorageVersion)
}

func syncMetadataPathVersion(key, section, hash string, version int) (string, error) {
	directory, err := syncMetadataDirectory(key)
	if err != nil || !syncCacheHash(hash) || !slices.Contains(append([]string{"versions"}, syncPartNames...), section) {
		return "", errors.New("同步元数据引用无效")
	}
	suffix := ".json.gz"
	if version == 2 {
		suffix = ".json"
	}
	return path.Join(directory, section, hash+suffix), nil
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
	return decodeSyncDeviceIndexVersion(body, device, syncStorageVersion, read)
}

func decodeSyncDeviceIndexVersion(body []byte, device string, version int, read syncMetadataReader) (*syncDeviceCatalog, error) {
	catalog, err := decodeSyncDeviceHeaderVersion(body, device, version)
	if err != nil {
		return nil, err
	}
	for key, hash := range catalog.Index.Entries {
		name, err := syncMetadataPathVersion(key, "versions", hash, version)
		if err != nil {
			return nil, err
		}
		body, err := read(name, hash)
		if err != nil {
			return nil, err
		}
		var head syncDocumentHead
		if decodeSyncJSON(body, &head) != nil || head.Format != "neoread-document" || head.Version != version || head.Key != key || head.Device != device || len(head.Parts) != len(syncPartNames) {
			return nil, errors.New("云端论文版本无效")
		}
		catalog.Known[name] = true
		document := emptySyncDocument()
		for phase, partName := range syncPartNames {
			partHash := head.Parts[partName]
			file, err := syncMetadataPathVersion(key, partName, partHash, version)
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
	manifest := syncManifest{Format: "neoread-webdav", Version: 1, Device: device, Entries: catalog.Versions}
	if err := validateSyncManifest(&manifest, device); err != nil {
		return nil, err
	}
	return catalog, nil
}

func (s *Server) metadataReader(ctx context.Context, dav *davClient, config webdavConfig) syncMetadataReader {
	return func(name, hash string) ([]byte, error) {
		cache := filepath.Join(filepath.Dir(s.syncStatePath(config)), "metadata", filepath.FromSlash(name))
		packed, err := os.ReadFile(cache)
		if err == nil {
			if body, e := unpackSyncMetadata(packed, syncManifestLimit); e == nil && syncBytesHash(body) == hash {
				return body, nil
			}
		}
		packed, err = dav.get(ctx, name, syncManifestLimit)
		if err != nil {
			return nil, err
		}
		body, err := unpackSyncMetadata(packed, syncManifestLimit)
		if err != nil {
			return nil, err
		}
		if syncBytesHash(body) != hash {
			return nil, errors.New("云端论文元数据校验失败")
		}
		if err = atomicFile(cache, packed); err != nil {
			return nil, err
		}
		return body, nil
	}
}

func packSyncMetadata(body []byte) ([]byte, error) {
	var buffer bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err = writer.Write(body); err != nil {
		return nil, err
	}
	if err = writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func unpackSyncMetadata(packed []byte, limit int64) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return nil, errors.New("同步元数据压缩格式无效")
	}
	defer reader.Close()
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, errors.New("同步元数据未完整解压或超过格式上限")
	}
	return body, nil
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
	packed, err := packSyncMetadata(body)
	if err != nil {
		return "", err
	}
	if err = dav.put(ctx, name, packed); err != nil {
		return "", err
	}
	cache := filepath.Join(filepath.Dir(s.syncStatePath(config)), "metadata", filepath.FromSlash(name))
	if err = atomicFile(cache, packed); err != nil {
		return "", err
	}
	catalog.Known[name] = true
	return hash, nil
}

func (s *Server) publishSyncVersion(ctx context.Context, dav *davClient, config webdavConfig, catalog *syncDeviceCatalog, key string, version *syncVersion) error {
	head := syncDocumentHead{Format: "neoread-document", Version: syncStorageVersion, Key: key, Clock: version.Clock, Deleted: version.Deleted, Device: version.Device, Name: version.Name, Updated: version.Updated, Parts: map[string]string{}}
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
	return &syncDeviceCatalog{Index: syncDeviceIndex{Format: "neoread-webdav", Version: syncStorageVersion, Device: device, Name: name, Entries: map[string]string{}}, Versions: map[string]*syncVersion{}, Known: map[string]bool{}}
}

func (catalog *syncDeviceCatalog) keys() []string {
	return slices.Sorted(maps.Keys(catalog.Index.Entries))
}

func (catalog *syncDeviceCatalog) version(key string, read syncMetadataReader) (*syncVersion, error) {
	if version := catalog.Versions[key]; version != nil {
		return version, nil
	}
	var version *syncVersion
	if hash := catalog.Index.Entries[key]; hash != "" {
		index := catalog.Index
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
	if version != nil {
		catalog.Versions[key] = version
	}
	return version, nil
}

func readSyncCachedCatalog(root *os.Root, name, device string) (*syncDeviceCatalog, error) {
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
	if header.Version == 1 {
		var manifest syncManifest
		if err = readSyncCacheJSON(root, name, &manifest); err != nil {
			return nil, err
		}
		return &syncDeviceCatalog{Versions: manifest.Entries, Known: map[string]bool{}}, validateSyncManifest(&manifest, device)
	}
	if header.Version != 2 && header.Version != syncStorageVersion {
		return nil, errors.New("同步缓存版本无效")
	}
	read := func(remote, hash string) ([]byte, error) {
		file := filepath.Join(filepath.Dir(filepath.Dir(name)), "metadata", filepath.FromSlash(remote))
		info, err := root.Lstat(file)
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("同步元数据缓存缺失")
		}
		body, err := root.ReadFile(file)
		if err == nil && strings.HasSuffix(file, ".gz") {
			body, err = unpackSyncMetadata(body, syncManifestLimit)
		}
		if err != nil || syncBytesHash(body) != hash {
			return nil, errors.New("同步元数据缓存校验失败")
		}
		return body, nil
	}
	catalog, err := decodeSyncDeviceIndexVersion(body, device, header.Version, read)
	if err != nil {
		return nil, err
	}
	return catalog, nil
}
