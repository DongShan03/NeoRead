package neoread

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
)

const syncStorageSchema = `
 CREATE TABLE IF NOT EXISTS sync_receiving(key TEXT PRIMARY KEY,body TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS sync_receipts(key TEXT PRIMARY KEY,body TEXT NOT NULL);
`

// A received original is usable immediately. Keep the unreceived remote
// records separate from its visible rows: their absence is not a local edit.
// This marker is committed with the file-install journal and SQLite writes.
type syncReceiving struct {
	Connection string        `json:"connection"`
	Remote     *syncDocument `json:"remote"`
	Baseline   *syncDocument `json:"baseline"`
	Clock      syncClock     `json:"clock"`
	Parents    syncClock     `json:"parents"`
}

func readSyncReceiving(ctx context.Context, q syncQuery) (map[string]*syncReceiving, error) {
	rows, err := q.QueryContext(ctx, "SELECT key,body FROM sync_receiving")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]*syncReceiving{}
	for rows.Next() {
		var key, body string
		var receiving syncReceiving
		if err = rows.Scan(&key, &body); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(key, "document/") || !safeSyncID(strings.TrimPrefix(key, "document/")) || decodeSyncJSON([]byte(body), &receiving) != nil || !syncCacheHash(receiving.Connection) || receiving.Remote == nil || receiving.Baseline == nil || receiving.Clock == nil || receiving.Parents == nil {
			return nil, errors.New("未完成的同步记录无法读取，已保留本地内容")
		}
		result[key] = &receiving
	}
	return result, rows.Err()
}

func (snapshot *syncSnapshot) observed(key string) *syncDocument {
	if snapshot.Observed != nil {
		return snapshot.Observed[key]
	}
	return snapshot.Documents[key]
}

func (snapshot *syncSnapshot) restoreReceiving(state *syncLocalState) error {
	for key, receiving := range snapshot.Receiving {
		owner := strings.TrimPrefix(key, "document/")
		if receiving.Connection != state.Connection {
			// A paused connection still owns this unfinished receive. Do not
			// publish its temporary absence through a different connection.
			snapshot.Busy[owner] = true
			if before := state.Base[key]; before != nil {
				snapshot.Documents[key] = before
			} else {
				delete(snapshot.Documents, key)
			}
			continue
		}
		state.Clock.include(receiving.Clock)
		state.Base[key] = receiving.Baseline
		state.BaseClocks[key] = receiving.Parents
		actual := snapshot.observed(key)
		if snapshot.Busy[owner] || actual == nil {
			continue // A local permanent deletion remains a deletion.
		}
		snapshot.Documents[key] = overlaySyncReceiving(receiving.Baseline, actual, receiving.Remote)
	}
	return nil
}

// Fill only the as-yet unseen remote content. A conflicting local note stays
// a local note, with its pre-receive causal parents; normal conflict resolution
// can then offer both versions instead of silently treating it as an overwrite.
func overlaySyncReceiving(base, actual, remote *syncDocument) *syncDocument {
	result := copySyncDocument(remote)
	keys := map[string]bool{}
	for key := range base.Records {
		keys[key] = true
	}
	for key := range actual.Records {
		keys[key] = true
	}
	for key := range keys {
		if syncHash(base.Records[key]) == syncHash(actual.Records[key]) {
			continue
		}
		one := func(record syncRecord) *syncDocument {
			value := emptySyncDocument()
			if record != nil {
				value.Records[key] = record
			}
			return value
		}
		merged, ok := mergeSyncDocument(one(base.Records[key]), one(actual.Records[key]), one(remote.Records[key]))
		chosen := actual.Records[key]
		if ok {
			chosen = merged.Records[key]
		}
		if chosen == nil {
			delete(result.Records, key)
		} else {
			result.Records[key] = chosen
		}
	}
	fileKeys := map[string]bool{}
	for name := range base.Files {
		fileKeys[name] = true
	}
	for name := range actual.Files {
		fileKeys[name] = true
	}
	for name := range fileKeys {
		before, had := base.Files[name]
		current, has := actual.Files[name]
		if had == has && syncHash(before) == syncHash(current) {
			continue
		}
		if has {
			result.Files[name] = current
		} else {
			delete(result.Files, name)
		}
	}
	return result
}

func syncFilePhase(name string) int {
	if strings.HasPrefix(name, "papers/") || strings.HasPrefix(name, "books/") {
		return 0
	}
	if strings.HasPrefix(name, "cache/") {
		return 1 // Translation layout and its images must arrive together.
	}
	return 2
}

func syncFileOrder(files map[string]syncFile) []string {
	names := slices.Collect(maps.Keys(files))
	slices.SortFunc(names, func(a, b string) int {
		if delta := syncFilePhase(a) - syncFilePhase(b); delta != 0 {
			return delta
		}
		return strings.Compare(a, b)
	})
	return names
}

func syncRecordPhase(record syncRecord) int {
	if len(record["documents"]) > 0 {
		return 0
	}
	for _, table := range []string{"translations", "translation_previous", "search_index"} {
		if len(record[table]) > 0 {
			return 1
		}
	}
	return 2
}

func syncReceivePhase(before, remote *syncDocument, phase int) *syncDocument {
	target := emptySyncDocument()
	if before != nil && phase < 2 {
		target.Records = maps.Clone(before.Records)
		target.Files = maps.Clone(before.Files)
	}
	for key, record := range remote.Records {
		if syncRecordPhase(record) <= phase {
			target.Records[key] = record
		}
	}
	for name, file := range remote.Files {
		if syncFilePhase(name) <= phase {
			target.Files[name] = file
		}
	}
	return target
}

func syncPDF(doc *syncDocument) bool {
	for _, record := range doc.RecordsSafe() {
		if len(record["documents"]) == 1 {
			return fmtSyncString(record["documents"][0]["type"]) == "pdf"
		}
	}
	return false
}

func copySyncDocument(doc *syncDocument) *syncDocument {
	if doc == nil {
		return nil
	}
	// Records are JSON data. Keep immutable baselines independent of subsequent
	// local snapshots and normalization performed by an install.
	var result syncDocument
	body, _ := json.Marshal(doc)
	_ = decodeSyncJSON(body, &result)
	return &result
}
