package neoread

import (
	"context"
	"errors"
	"maps"
	"strings"
)

// Commit causal metadata with the visible records. The filesystem state index
// may be saved later; a crash must not turn an already chosen note merge back
// into an unresolved conflict.
type syncCommitReceipt struct {
	Connection string        `json:"connection"`
	Base       *syncDocument `json:"base"`
	Clock      syncClock     `json:"clock"`
	Own        *syncVersion  `json:"own,omitempty"`
}

func readSyncReceipts(ctx context.Context, q syncQuery) (map[string]*syncCommitReceipt, error) {
	rows, err := q.QueryContext(ctx, "SELECT key,body FROM sync_receipts")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]*syncCommitReceipt{}
	for rows.Next() {
		var key, body string
		var receipt syncCommitReceipt
		if err = rows.Scan(&key, &body); err != nil {
			return nil, err
		}
		if key != "library" && (!strings.HasPrefix(key, "document/") || !safeSyncID(strings.TrimPrefix(key, "document/"))) || decodeSyncJSON([]byte(body), &receipt) != nil || !syncCacheHash(receipt.Connection) || receipt.Clock == nil {
			return nil, errors.New("同步提交记录无法读取，已保留书库数据")
		}
		result[key] = &receipt
	}
	return result, rows.Err()
}

func applySyncReceipt(state *syncLocalState, key string, receipt *syncCommitReceipt) {
	if receipt.Connection != state.Connection {
		return
	}
	state.Clock.include(receipt.Clock)
	if receipt.Base == nil {
		delete(state.Base, key)
	} else {
		state.Base[key] = receipt.Base
	}
	state.BaseClocks[key] = maps.Clone(receipt.Clock)
	if receipt.Own != nil {
		version := receipt.Own
		if version.Device != state.Device {
			version = newSyncVersion(state, version.Body, version.Body, version.Name, version.Clock)
			version.Deleted = receipt.Own.Deleted
		}
		state.Own[key] = version
	}
	delete(state.Choices, key)
}

func (s *Server) clearSyncReceipts(connection string, key *string) error {
	query := "DELETE FROM sync_receipts WHERE json_extract(body,'$.connection')=?"
	args := []any{connection}
	if key != nil {
		query += " AND key=?"
		args = append(args, *key)
	}
	_, err := s.Store.DB.Exec(query, args...)
	return err
}
