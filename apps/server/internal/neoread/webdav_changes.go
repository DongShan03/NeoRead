package neoread

import (
	"context"
	"database/sql"
	"strings"
)

type syncRevisionReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func syncLocalRevision(ctx context.Context, q syncRevisionReader) (int64, error) {
	var revision int64
	err := q.QueryRowContext(ctx, "SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM settings WHERE key='sync:local-revision'),0)").Scan(&revision)
	return revision, err
}

// Count changes to replicated records in the same SQLite transaction as the
// write. Status polling reads one counter, without scanning the book library.
func installSyncChangeTracking(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, name := range syncTables {
		if name == "search_index" { // Derived FTS virtual tables cannot have triggers.
			continue
		}
		for _, operation := range []string{"INSERT", "UPDATE", "DELETE"} {
			condition := "1"
			if name == "settings" {
				prefix := "NEW"
				if operation == "DELETE" {
					prefix = "OLD"
				}
				section := func(prefix, name string) string {
					return "json_extract(CASE WHEN json_valid(" + prefix + ".value) THEN " + prefix + ".value ELSE '{}' END,'$." + name + "')"
				}
				condition = prefix + ".key='preferences:library' AND (NOT json_valid(" + prefix + ".value) OR " + section(prefix, "papers") + " IS NOT NULL OR " + section(prefix, "books") + " IS NOT NULL)"
				if operation == "UPDATE" {
					condition = "NEW.key='preferences:library' AND (NOT json_valid(OLD.value) OR NOT json_valid(NEW.value) OR " + section("OLD", "papers") + " IS NOT " + section("NEW", "papers") + " OR " + section("OLD", "books") + " IS NOT " + section("NEW", "books") + ")"
				}
			}
			_, err = tx.Exec("CREATE TRIGGER IF NOT EXISTS webdav_" + name + "_" + strings.ToLower(operation) + " AFTER " + operation + " ON " + name + " WHEN " + condition + " BEGIN INSERT INTO settings(key,value) VALUES('sync:local-revision','1') ON CONFLICT(key) DO UPDATE SET value=CAST(value AS INTEGER)+1; END")
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Server) syncChangeStatus() (dirty, failed bool, err error) {
	var revision, synced int64
	var outcome string
	err = s.Store.DB.QueryRow(`SELECT
 COALESCE((SELECT CAST(value AS INTEGER) FROM settings WHERE key='sync:local-revision'),0),
 COALESCE((SELECT CAST(value AS INTEGER) FROM settings WHERE key='sync:synced-revision'),-1),
 COALESCE((SELECT value FROM settings WHERE key='sync:last-result'),'')`).Scan(&revision, &synced, &outcome)
	return revision != synced, outcome == "failed", err
}

func (s *Server) confirmSyncRevision(ctx context.Context, revision int64) error {
	tx, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := syncLocalRevision(ctx, tx)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('sync:synced-revision',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", revision)
	if err == nil && current == revision {
		_, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('sync:last-result','success') ON CONFLICT(key) DO UPDATE SET value=excluded.value")
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
