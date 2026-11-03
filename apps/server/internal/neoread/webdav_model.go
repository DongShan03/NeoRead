package neoread

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"time"
)

type webdavConfig struct {
	LibraryConfigured bool   `json:"libraryConfigured"`
	RemoteID          string `json:"remoteId,omitempty"`
	IntervalMinutes   int    `json:"intervalMinutes"`
	Account           string `json:"account"`
	Password          string `json:"password,omitempty"`
	PasswordSet       bool   `json:"passwordSet"`
	Folder            string `json:"folder"`
	DeviceName        string `json:"deviceName"`
	Enabled           bool   `json:"enabled"`
}
type syncClock map[string]uint64

func (c syncClock) covers(other syncClock) bool {
	for device, value := range other {
		if c[device] < value {
			return false
		}
	}
	return true
}
func (c syncClock) include(other syncClock) {
	for device, value := range other {
		c[device] = max(c[device], value)
	}
}

type syncRow map[string]any

// Ordinary records contain one row. A chat record contains a session and its
// ordered messages, so concurrent edits cannot scramble a transcript.
type syncRecord map[string][]syncRow
type syncChunk struct {
	Hash string `json:"hash"`
	Size int    `json:"size"`
}
type syncFile struct {
	Size   int64       `json:"size"`
	Chunks []syncChunk `json:"chunks"`
}
type syncDocument struct {
	Records map[string]syncRecord `json:"records"`
	Files   map[string]syncFile   `json:"files"`
}
type syncVersion struct {
	Clock   syncClock     `json:"clock"`
	Deleted bool          `json:"deleted,omitempty"`
	Body    *syncDocument `json:"body"`
	Device  string        `json:"device"`
	Name    string        `json:"name"`
	Updated string        `json:"updated"`
}
type syncManifest struct {
	Format  string                  `json:"format"`
	Version int                     `json:"version"`
	Device  string                  `json:"device"`
	Entries map[string]*syncVersion `json:"entries"`
}
type syncFingerprint struct {
	Size     int64    `json:"size"`
	Modified int64    `json:"modified"`
	File     syncFile `json:"file"`
}
type syncRemoteCache struct {
	ETag string `json:"etag"`
}
type syncChoice struct {
	Conflict string `json:"conflict"`
	Version  string `json:"version"`
	KeepBoth bool   `json:"keepBoth,omitempty"`
}

// The reader has one note per document. Keep each distinct version in a named
// Markdown section, then merge the other records using the normal rules.
func keepSyncNotes(base *syncDocument, versions []*syncVersion) (*syncDocument, bool) {
	if len(versions) < 2 {
		return nil, false
	}
	noteKey := ""
	parts := []string{}
	seen := map[string]bool{}
	var template syncRow
	for _, version := range versions {
		if version.Deleted || version.Body == nil {
			return nil, false
		}
		text := ""
		for key, record := range version.Body.Records {
			if rows := record["document_notes"]; len(rows) == 1 {
				noteKey, template = key, rows[0]
				text = fmtSyncString(rows[0]["body"])
			}
		}
		if seen[text] {
			continue
		}
		seen[text] = true
		name := strings.NewReplacer("\r", " ", "\n", " ").Replace(version.Name)
		parts = append(parts, "## "+name+" · "+version.Updated+"\n\n"+text)
	}
	if noteKey == "" || len(parts) < 2 {
		return nil, false
	}
	row := syncRow{}
	for key, value := range template {
		row[key] = value
	}
	row["body"], row["updated_at"] = strings.Join(parts, "\n\n---\n\n"), now()
	if len([]rune(fmtSyncString(row["body"]))) > 100000 {
		return nil, false
	}
	var result *syncDocument
	for _, version := range versions {
		copy := &syncDocument{Records: map[string]syncRecord{}, Files: version.Body.Files}
		for key, record := range version.Body.Records {
			copy.Records[key] = record
		}
		copy.Records[noteKey] = syncRecord{"document_notes": {row}}
		if result == nil {
			result = copy
			continue
		}
		var ok bool
		result, ok = mergeSyncDocument(base, result, copy)
		if !ok {
			return nil, false
		}
	}
	return result, true
}

type syncLocalState struct {
	Device     string                     `json:"device"`
	Clock      syncClock                  `json:"clock"`
	Own        map[string]*syncVersion    `json:"own"`
	Base       map[string]*syncDocument   `json:"base"`
	BaseClocks map[string]syncClock       `json:"baseClocks"`
	Files      map[string]syncFingerprint `json:"files"`
	Uploaded   map[string]bool            `json:"uploaded"`
	Remotes    map[string]syncRemoteCache `json:"remotes"`
	Choices    map[string]syncChoice      `json:"choices"`
	LastSync   string                     `json:"lastSync,omitempty"`
}

func newSyncState() *syncLocalState {
	return &syncLocalState{Device: id(), Clock: syncClock{}, Own: map[string]*syncVersion{},
		Base: map[string]*syncDocument{}, BaseClocks: map[string]syncClock{}, Files: map[string]syncFingerprint{}, Uploaded: map[string]bool{},
		Remotes: map[string]syncRemoteCache{}, Choices: map[string]syncChoice{}}
}
func syncHash(v any) string            { b, _ := json.Marshal(v); return syncBytesHash(b) }
func syncBytesHash(body []byte) string { h := sha256.Sum256(body); return hex.EncodeToString(h[:]) }
func syncID(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func safeSyncID(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 256 && !strings.ContainsAny(value, "/\\\x00\r\n")
}
func sameSyncValue(a, b *syncVersion) bool {
	return a.Deleted == b.Deleted && (a.Deleted || syncHash(a.Body) == syncHash(b.Body))
}
func syncCandidates(current []*syncVersion, incoming *syncVersion) []*syncVersion {
	for _, previous := range current {
		if previous.Clock.covers(incoming.Clock) {
			return current
		}
	}
	result := []*syncVersion{}
	for _, previous := range current {
		if !incoming.Clock.covers(previous.Clock) {
			result = append(result, previous)
		}
	}
	result = append(result, incoming)
	slices.SortFunc(result, func(a, b *syncVersion) int { return strings.Compare(a.Device, b.Device) })
	return result
}
func newSyncVersion(state *syncLocalState, body, previous *syncDocument, name string, parents ...syncClock) *syncVersion {
	state.Clock[state.Device]++
	clock := syncClock{}
	if len(parents) == 0 {
		clock.include(state.Clock)
	} else {
		for _, parent := range parents {
			clock.include(parent)
		}
	}
	clock[state.Device] = state.Clock[state.Device]
	deleted := body == nil
	if deleted {
		body = previous
	}
	return &syncVersion{Clock: clock, Body: body, Deleted: deleted, Device: state.Device, Name: name, Updated: now()}
}

// Merge changes to different records; retain both versions when the same note,
// annotation, transcript or parsed asset was changed independently.
func mergeSyncDocument(base, local, remote *syncDocument) (*syncDocument, bool) {
	if local == nil || remote == nil {
		return nil, false
	}
	if base == nil {
		base = &syncDocument{}
	}
	result := &syncDocument{Records: map[string]syncRecord{}, Files: map[string]syncFile{}}
	keys := map[string]bool{}
	for _, records := range []map[string]syncRecord{base.Records, local.Records, remote.Records} {
		for key := range records {
			keys[key] = true
		}
	}
	for key := range keys {
		b, l, r := base.Records[key], local.Records[key], remote.Records[key]
		var chosen syncRecord
		switch {
		case syncHash(l) == syncHash(r), syncHash(r) == syncHash(b):
			chosen = l
		case syncHash(l) == syncHash(b):
			chosen = r
		default:
			if len(l["document_notes"]) == 1 && len(r["document_notes"]) == 1 && l["document_notes"][0]["body"] == r["document_notes"][0]["body"] {
				chosen = l
				if syncTimeAfter(fmtSyncString(r["document_notes"][0]["updated_at"]), fmtSyncString(l["document_notes"][0]["updated_at"])) {
					chosen = r
				}
				break
			}
			if !strings.HasPrefix(key, "documents/") || l == nil || r == nil {
				return nil, false
			}
			row, ok := mergeSyncDocumentRow(b["documents"], l["documents"], r["documents"])
			if !ok {
				return nil, false
			}
			chosen = syncRecord{"documents": {row}}
		}
		if chosen != nil {
			result.Records[key] = chosen
		}
	}
	fileKeys := map[string]bool{}
	for _, files := range []map[string]syncFile{base.Files, local.Files, remote.Files} {
		for key := range files {
			fileKeys[key] = true
		}
	}
	for key := range fileKeys {
		b, bok := base.Files[key]
		l, lok := local.Files[key]
		r, rok := remote.Files[key]
		switch {
		case lok == rok && reflect.DeepEqual(l, r), rok == bok && reflect.DeepEqual(r, b):
			if lok {
				result.Files[key] = l
			}
		case lok == bok && reflect.DeepEqual(l, b):
			if rok {
				result.Files[key] = r
			}
		default:
			return nil, false
		}
	}
	return result, true
}
func mergeSyncDocumentRow(base, local, remote []syncRow) (syncRow, bool) {
	if len(local) != 1 || len(remote) != 1 {
		return nil, false
	}
	b := syncRow{}
	if len(base) == 1 {
		b = base[0]
	}
	l, r := local[0], remote[0]
	result := syncRow{}
	newest := l
	if syncTimeAfter(fmtSyncString(r["last_opened_at"]), fmtSyncString(l["last_opened_at"])) {
		newest = r
	}
	reading := map[string]bool{"progress": true, "percentage": true, "last_opened_at": true, "body_read_position": true}
	for key, value := range l {
		if key == "created_at" {
			result[key] = value
			if syncTimeAfter(fmtSyncString(value), fmtSyncString(r[key])) {
				result[key] = r[key]
			}
			continue
		}
		if reading[key] {
			result[key] = newest[key]
			if key == "body_read_position" {
				result[key] = max(syncNumber(l[key]), syncNumber(r[key]))
			}
			continue
		}
		switch {
		case syncHash(value) == syncHash(r[key]), syncHash(r[key]) == syncHash(b[key]):
			result[key] = value
		case syncHash(value) == syncHash(b[key]):
			result[key] = r[key]
		default:
			return nil, false
		}
	}
	return result, true
}
func syncTimeAfter(a, b string) bool {
	ta, ea := time.Parse(time.RFC3339Nano, a)
	tb, eb := time.Parse(time.RFC3339Nano, b)
	if ea == nil && eb == nil {
		return ta.After(tb)
	}
	return a > b
}
func fmtSyncString(value any) string { s, _ := value.(string); return s }
func syncNumber(value any) float64 {
	switch n := value.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}
func decodeSyncJSON(body []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	d.DisallowUnknownFields()
	if d.Decode(target) != nil {
		return errors.New("同步数据格式无效，未修改本地书库")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("同步数据包含多余内容")
	}
	return nil
}
func validateSyncManifest(m *syncManifest, device string) error {
	if m.Format != "neoread-webdav" || m.Version != 1 || m.Device != device || m.Entries == nil {
		return errors.New("云端同步格式与当前版本不兼容")
	}
	for key, entry := range m.Entries {
		if key == "library" && entry != nil && entry.Deleted {
			return errors.New("云端书库设置不能被整体删除")
		}
		if entry == nil || entry.Body == nil || entry.Device != device || !syncID(device) || len(entry.Clock) == 0 {
			return errors.New("云端同步版本无效")
		}
		if key != "library" && (!strings.HasPrefix(key, "document/") || !safeSyncID(strings.TrimPrefix(key, "document/"))) {
			return errors.New("云端文档标识无效")
		}
		for id, value := range entry.Clock {
			if !syncID(id) || value == 0 {
				return errors.New("云端同步顺序无效")
			}
		}
		if _, err := time.Parse(time.RFC3339Nano, entry.Updated); err != nil {
			return errors.New("云端同步时间无效")
		}
	}
	return nil
}
