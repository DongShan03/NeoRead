package neoread

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gofrs/flock"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func legacyFixture(t *testing.T, external bool) (string, string, string) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old, current := filepath.Join(parent, "Reader"), filepath.Join(parent, "NeoRead")
	lib := filepath.Join(old, "library")
	if external {
		lib = filepath.Join(parent, "My papers")
	}
	if err = os.MkdirAll(old, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(lib)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`INSERT INTO settings VALUES('reader','{"appearance":"dark"}'); INSERT INTO settings VALUES('connector:secret','paired'); INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at,percentage) VALUES('paper','pdf','Paper','Author',3,'now','now',0.5); INSERT INTO document_notes VALUES('paper','my note','now'); INSERT INTO annotations VALUES('mark','paper','{"text":"keep"}');`); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(lib, "papers", "paper.pdf"), []byte("pdf"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(lib, "neoread.sqlite"), filepath.Join(lib, "reader.sqlite")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(old, "proxy-settings.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if external {
		if err = atomicMigrationJSON(filepath.Join(old, "library-location.json"), map[string]string{"directory": lib}); err != nil {
			t.Fatal(err)
		}
	}
	return old, current, lib
}
func assertMigrated(t *testing.T, root string) {
	t.Helper()
	s, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	for key, want := range map[string]string{"neoread": "{\"appearance\":\"dark\"}", "connector:secret": "paired"} {
		var got string
		if err = s.DB.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&got); err != nil || got != want {
			t.Fatal(key, got, err)
		}
	}
	var note string
	if err = s.DB.QueryRow("SELECT body FROM document_notes WHERE document_id='paper'").Scan(&note); err != nil || note != "my note" {
		t.Fatal(note, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "papers", "paper.pdf")); err != nil || string(data) != "pdf" {
		t.Fatal(string(data), err)
	}
}
func TestReaderMigrationCopiesDefaultAndExternalOnce(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "external"}[external], func(t *testing.T) {
			old, current, lib := legacyFixture(t, external)
			before, _, _ := treeDigest(old)
			state, err := MigrateReaderData(old, current)
			if err != nil {
				t.Fatal(err)
			}
			if state.Status != "completed" || state.LegacyCleanup.Status != "pending" {
				t.Fatal(state)
			}
			assertMigrated(t, state.Library)
			after, _, _ := treeDigest(old)
			if before != after || !pathExists(filepath.Join(lib, "reader.sqlite")) {
				t.Fatal("source modified")
			}
			// An updated application must not replay the old library over new edits.
			if err = os.WriteFile(filepath.Join(state.Library, "papers", "paper.pdf"), []byte("new"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = MigrateReaderData(old, current); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(filepath.Join(state.Library, "papers", "paper.pdf"))
			if string(data) != "new" {
				t.Fatal("repeated migration")
			}
		})
	}
}
func TestReaderMigrationFreshAndExistingInstallAreTerminal(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	old, current := filepath.Join(root, "Reader"), filepath.Join(root, "NeoRead")
	state, err := MigrateReaderData(old, current)
	if err != nil || state.Status != "not-needed" {
		t.Fatal(state, err)
	}
	os.MkdirAll(filepath.Join(old, "library"), 0700)
	state, err = MigrateReaderData(old, current)
	if err != nil || state.Reason != "fresh-install" {
		t.Fatal(state, err)
	}
	old, current, _ = legacyFixture(t, false)
	os.MkdirAll(filepath.Join(current, "library"), 0700)
	state, err = MigrateReaderData(old, current)
	if err != nil || state.Reason != "existing-install" {
		t.Fatal(state, err)
	}
}
func TestReaderMigrationFailsClosedForInvalidPointerAndOfflineDrive(t *testing.T) {
	for _, pointer := range []string{"invalid", `{"directory":"relative"}`, `{"directory":"/missing/drive/books"}`} {
		old, current, _ := legacyFixture(t, false)
		os.WriteFile(filepath.Join(old, "library-location.json"), []byte(pointer), 0600)
		if _, err := MigrateReaderData(old, current); err == nil {
			t.Fatal("accepted", pointer)
		}
		if pathExists(filepath.Join(current, "library")) {
			t.Fatal("started empty library")
		}
	}
}
func TestReaderMigrationRequiresOldLibraryLock(t *testing.T) {
	old, current, lib := legacyFixture(t, false)
	lock := flock.New(filepath.Join(lib, "reader.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateReaderData(old, current); err == nil {
		t.Fatal("migrated a running library")
	}
	lock.Unlock()
	if _, err := MigrateReaderData(old, current); err != nil {
		t.Fatal(err)
	}
}
func TestReaderMigrationRefusesCorruptStateAndDestinationCollision(t *testing.T) {
	old, current, lib := legacyFixture(t, true)
	os.MkdirAll(filepath.Join(filepath.Dir(lib), "NeoRead Library"), 0700)
	if _, err := MigrateReaderData(old, current); err == nil {
		t.Fatal("overwrote destination")
	}
	os.WriteFile(filepath.Join(current, "migrations.json"), []byte("broken"), 0600)
	if _, err := MigrateReaderData(old, current); err == nil {
		t.Fatal("ignored corrupt state")
	}
}
func TestReaderMigrationKeepsUncheckpointedWAL(t *testing.T) {
	old, current, lib := legacyFixture(t, false)
	db, err := sql.Open("sqlite", libraryDatabaseURI(filepath.Join(lib, "reader.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; INSERT INTO settings VALUES('wal-only','committed')"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(lib, "reader.sqlite-wal")); err != nil || info.Size() == 0 {
		t.Fatal("no WAL fixture", err)
	}
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(state.Library)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	var got string
	if err = s.DB.QueryRow("SELECT value FROM settings WHERE key='wal-only'").Scan(&got); err != nil || got != "committed" {
		t.Fatal(got, err)
	}
}
func TestReaderMigrationResumesAfterActivationBeforeCompletion(t *testing.T) {
	old, current, _ := legacyFixture(t, true)
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	file, _, _ := readMigration(current)
	state.Status = "verified"
	state.LegacyCleanup = nil
	if err = saveMigration(current, file, state); err != nil {
		t.Fatal(err)
	}
	state, err = MigrateReaderData(old, current)
	if err != nil || state.Status != "completed" {
		t.Fatal(state, err)
	}
	assertMigrated(t, state.Library)
}
func TestReaderMigrationRestartsInterruptedCopy(t *testing.T) {
	old, current, _ := legacyFixture(t, false)
	// Stop conversion after an otherwise complete directory copy.
	os.WriteFile(filepath.Join(old, "library", "reader.sqlite"), []byte("corrupt"), 0600)
	if _, err := MigrateReaderData(old, current); err == nil {
		t.Fatal("accepted corrupt DB")
	}
	file, state, err := readMigration(current)
	if err != nil || state.Status != "copying" {
		t.Fatal(state, err)
	}
	if len(file.Migrations) != 1 {
		t.Fatal(file)
	}
	// The source is untouched, and no incomplete library has been activated.
	if pathExists(filepath.Join(current, "library")) {
		t.Fatal("activated incomplete library")
	}
}
func TestReaderCleanupRequiresExplicitChoiceAndPreservesExternalLibrary(t *testing.T) {
	old, current, lib := legacyFixture(t, true)
	_, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	state, err := CleanupReaderData(old, current, "delete")
	if err != nil {
		t.Fatal(err)
	}
	if state.LegacyCleanup.Status != "deleted" || pathExists(old) || !pathExists(filepath.Join(lib, "reader.sqlite")) {
		t.Fatal("wrong deletion boundary")
	}
	if _, err = MigrateReaderData(old, current); err != nil {
		t.Fatal(err)
	}
}
func TestReaderCleanupKeepAndChangedSourceAreNeverRetried(t *testing.T) {
	for _, choice := range []string{"keep", "delete"} {
		old, current, _ := legacyFixture(t, false)
		_, err := MigrateReaderData(old, current)
		if err != nil {
			t.Fatal(err)
		}
		if choice == "delete" {
			os.WriteFile(filepath.Join(old, "library", "new-note"), []byte("do not delete"), 0600)
		}
		_, err = CleanupReaderData(old, current, choice)
		if choice == "delete" && err == nil {
			t.Fatal("deleted changed source")
		}
		if !pathExists(old) {
			t.Fatal("deleted original")
		}
		_, state, err := readMigration(current)
		if err != nil || state.LegacyCleanup.Status == "pending" {
			t.Fatal(state, err)
		}
		if _, err = CleanupReaderData(old, current, "delete"); err != nil {
			t.Fatal(err)
		}
		if !pathExists(old) {
			t.Fatal("retried cleanup")
		}
	}
}
func TestReaderMigrationPreservesOtherMigrationRecords(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	current := filepath.Join(root, "NeoRead")
	os.MkdirAll(current, 0700)
	atomicMigrationJSON(filepath.Join(current, "migrations.json"), migrationFile{SchemaVersion: 1, Migrations: map[string]json.RawMessage{"future": json.RawMessage(`{"status":"completed"}`)}})
	if _, err := MigrateReaderData(filepath.Join(root, "Reader"), current); err != nil {
		t.Fatal(err)
	}
	file, _, _ := readMigration(current)
	if len(file.Migrations) != 2 {
		t.Fatal(file)
	}
}

func TestReaderMigrationResumesPartiallyInstalledEntries(t *testing.T) {
	old, current, _ := legacyFixture(t, false)
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(current, migrationStage, "app")
	if err = os.Rename(filepath.Join(current, "proxy-settings.json"), filepath.Join(stage, "proxy-settings.json")); err != nil {
		t.Fatal(err)
	}
	file, _, _ := readMigration(current)
	state.Status = "verified"
	saveMigration(current, file, state)
	state, err = MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(current, "proxy-settings.json")) {
		t.Fatal("did not finish installation")
	}
	assertMigrated(t, state.Library)
}
func TestReaderMigrationRetriesCopyAfterFailure(t *testing.T) {
	old, current, lib := legacyFixture(t, false)
	database := filepath.Join(lib, "reader.sqlite")
	original, _ := os.ReadFile(database)
	os.WriteFile(database, []byte("bad database"), 0600)
	if _, err := MigrateReaderData(old, current); err == nil {
		t.Fatal("accepted broken database")
	}
	os.WriteFile(database, original, 0600)
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	assertMigrated(t, state.Library)
}
func TestReaderMigrationHandlesInternalCustomLibrary(t *testing.T) {
	old, current, lib := legacyFixture(t, false)
	custom := filepath.Join(old, "custom")
	if err := os.Rename(lib, custom); err != nil {
		t.Fatal(err)
	}
	atomicMigrationJSON(filepath.Join(old, "library-location.json"), map[string]string{"directory": custom})
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	if state.External || state.Library != filepath.Join(current, "custom") {
		t.Fatal(state)
	}
	assertMigrated(t, state.Library)
}
func TestReaderMigrationRefusesMissingOriginalFile(t *testing.T) {
	old, current, lib := legacyFixture(t, false)
	os.Remove(filepath.Join(lib, "papers", "paper.pdf"))
	if _, err := MigrateReaderData(old, current); err == nil {
		t.Fatal("accepted missing original file")
	}
	if pathExists(filepath.Join(current, "library")) {
		t.Fatal("activated incomplete data")
	}
}
func TestReaderMigrationRejectsSymlinks(t *testing.T) {
	old, current, _ := legacyFixture(t, false)
	if err := os.Symlink(t.TempDir(), filepath.Join(old, "library", "linked-data")); err != nil {
		t.Skip(err)
	}
	if _, err := MigrateReaderData(old, current); err == nil {
		t.Fatal("followed symlink")
	}
}
func TestReaderCleanupProtectsActiveLibraryAndRunningReader(t *testing.T) {
	for _, running := range []bool{false, true} {
		old, current, lib := legacyFixture(t, false)
		if _, err := MigrateReaderData(old, current); err != nil {
			t.Fatal(err)
		}
		if running {
			lock := flock.New(filepath.Join(lib, "reader.lock"))
			if err := lock.Lock(); err != nil {
				t.Fatal(err)
			}
			defer lock.Unlock()
		} else {
			atomicMigrationJSON(filepath.Join(current, "library-location.json"), map[string]string{"directory": lib})
		}
		if _, err := CleanupReaderData(old, current, "delete"); err == nil {
			t.Fatal("unsafe cleanup accepted")
		}
		if !pathExists(filepath.Join(lib, "reader.sqlite")) {
			t.Fatal("deleted active database")
		}
	}
}
func TestExistingNeoReadDatabaseRenamesWithoutImportingReader(t *testing.T) {
	old, current, lib := legacyFixture(t, false)
	if err := os.Rename(old, current); err != nil {
		t.Fatal(err)
	}
	lib = filepath.Join(current, "library")
	state, err := MigrateReaderData(old, current)
	if err != nil || state.Reason != "existing-install" {
		t.Fatal(state, err)
	}
	unlock, err := LockLibrary(lib)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	assertMigrated(t, lib)
	if !pathExists(filepath.Join(lib, "reader.sqlite")) {
		t.Fatal("removed existing database backup")
	}
}

func TestReaderMigrationAcceptsOlderSchemaWithoutCorpusTable(t *testing.T) {
	old, current, lib := legacyFixture(t, false)
	db, err := sql.Open("sqlite", libraryDatabaseURI(filepath.Join(lib, "reader.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("DROP TABLE document_corpora"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	assertMigrated(t, state.Library)
}
func TestReaderMigrationPreservesValidCorpusFingerprint(t *testing.T) {
	old, current, lib := legacyFixture(t, false)
	server := &Server{Store: &Store{Root: lib}}
	fingerprint, err := server.corpusFingerprint(Document{ID: "paper", Type: "pdf"})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", libraryDatabaseURI(filepath.Join(lib, "reader.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO document_corpora(document_id,revision,fingerprint,body) VALUES('paper','revision',?,'{}')", fingerprint); err != nil {
		t.Fatal(err)
	}
	db.Close()
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(state.Library)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	var actual string
	store.DB.QueryRow("SELECT fingerprint FROM document_corpora WHERE document_id='paper'").Scan(&actual)
	server.Store = store
	expected, err := server.corpusFingerprint(Document{ID: "paper", Type: "pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if actual != expected || actual == fingerprint {
		t.Fatal("corpus path not rebound", actual, expected)
	}
}

func TestReaderMigrationRejectsIncompleteVerifiedAndCompletedRecords(t *testing.T) {
	for _, status := range []string{"verified", "completed"} {
		old, current, _ := legacyFixture(t, false)
		state, err := MigrateReaderData(old, current)
		if err != nil {
			t.Fatal(err)
		}
		file, _, _ := readMigration(current)
		state.Status = status
		state.Entries = nil
		if err = saveMigration(current, file, state); err != nil {
			t.Fatal(err)
		}
		if _, err = MigrateReaderData(old, current); err == nil {
			t.Fatal("accepted incomplete state", status)
		}
	}
}

func TestReaderMigrationKeepsStartupDiagnosticsSeparate(t *testing.T) {
	old, current, _ := legacyFixture(t, false)
	for _, root := range []string{old, current} {
		if err := os.MkdirAll(filepath.Join(root, ".migration-logs"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".migration-logs", "desktop-session.jsonl"), []byte(root), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	if _, included := state.Entries[".migration-logs"]; included {
		t.Fatal("startup logs included in activation")
	}
	data, err := os.ReadFile(filepath.Join(current, ".migration-logs", "desktop-session.jsonl"))
	if err != nil || string(data) != current {
		t.Fatal("startup logs overwritten", err)
	}
	if !strings.Contains(output.String(), "migration.finish") {
		t.Fatal(output.String())
	}
}

func TestReaderMigrationLogsFailurePhaseWithoutSourceContent(t *testing.T) {
	old, current, lib := legacyFixture(t, false)
	if err := os.WriteFile(filepath.Join(lib, "reader.sqlite"), []byte("PRIVATE_DOCUMENT_SENTINEL"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	if _, err := MigrateReaderData(old, current); err == nil {
		t.Fatal("accepted corrupt database")
	}
	log := output.String()
	if !strings.Contains(log, `"msg":"migration.failed","phase":"convert_database"`) {
		t.Fatal(log)
	}
	if strings.Contains(log, "PRIVATE_DOCUMENT_SENTINEL") || strings.Contains(log, old) {
		t.Fatal("private source in logs")
	}
}

func writeMigrationCache(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name, "cache"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReaderMigrationIgnoresChromiumStartupCaches(t *testing.T) {
	old, current, _ := legacyFixture(t, false)
	for _, name := range []string{"GPUPersistentCache", "GraphiteDawnCache"} {
		writeMigrationCache(t, old, name, "old cache")
		writeMigrationCache(t, current, name, "new Chromium cache")
	}
	before, _, err := treeDigest(old)
	if err != nil {
		t.Fatal(err)
	}
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	assertMigrated(t, state.Library)
	for _, name := range []string{"GPUPersistentCache", "GraphiteDawnCache"} {
		if _, found := state.Entries[name]; found || pathExists(filepath.Join(current, migrationStage, "app", name)) {
			t.Fatal("copied runtime cache", name)
		}
		data, err := os.ReadFile(filepath.Join(current, name, "cache"))
		if err != nil || string(data) != "new Chromium cache" {
			t.Fatal("overwrote live cache", name, err)
		}
	}
	after, _, err := treeDigest(old)
	if err != nil || before != after {
		t.Fatal("modified source", err)
	}
	// Cleanup must use the same hash policy as this migration.
	if _, err := CleanupReaderData(old, current, "delete"); err != nil {
		t.Fatal(err)
	}
}

func TestReaderMigrationResumesVerifiedWithLegacyCaches(t *testing.T) {
	for _, external := range []bool{false, true} {
		for _, location := range []string{"staged", "installed", "missing"} {
			t.Run(fmt.Sprintf("external=%v/%s", external, location), func(t *testing.T) {
				old, current, _ := legacyFixture(t, external)
				state, err := MigrateReaderData(old, current)
				if err != nil {
					t.Fatal(err)
				}
				// Recreate a v0.9.0 verified journal: unversioned hashes include caches.
				encoded, _ := json.Marshal(state)
				var fields map[string]json.RawMessage
				json.Unmarshal(encoded, &fields)
				delete(fields, "digestVersion")
				delete(fields, "dataOnly")
				encoded, _ = json.Marshal(fields)
				state = new(ReaderMigration)
				if err := json.Unmarshal(encoded, state); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"GPUPersistentCache", "GraphiteDawnCache"} {
					writeMigrationCache(t, old, name, "legacy cache")
					hash, _, err := treeDigest(filepath.Join(old, name))
					if err != nil {
						t.Fatal(err)
					}
					state.Entries[name] = hash
					switch location {
					case "staged":
						writeMigrationCache(t, filepath.Join(current, migrationStage, "app"), name, "legacy cache")
						writeMigrationCache(t, current, name, "live cache")
					case "installed":
						writeMigrationCache(t, current, name, "cache changed after installation")
					}
				}
				state.SourceHash, _, err = treeDigest(old)
				if err != nil {
					t.Fatal(err)
				}
				// Also leave one real application entry pending activation.
				if err := os.Rename(filepath.Join(current, "proxy-settings.json"), filepath.Join(current, migrationStage, "app", "proxy-settings.json")); err != nil {
					t.Fatal(err)
				}
				file, _, _ := readMigration(current)
				state.Status, state.CompletedAt, state.LegacyCleanup = "verified", "", nil
				if err := saveMigration(current, file, state); err != nil {
					t.Fatal(err)
				}
				state, err = MigrateReaderData(old, current)
				if err != nil || state.Status != "completed" {
					t.Fatal(state, err)
				}
				assertMigrated(t, state.Library)
				if !pathExists(filepath.Join(current, "proxy-settings.json")) {
					t.Fatal("did not activate settings")
				}
				// Completed old journals and their cleanup hashes remain valid too.
				if _, err := MigrateReaderData(old, current); err != nil {
					t.Fatal(err)
				}
				if _, err := CleanupReaderData(old, current, "delete"); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestReaderMigrationDiagnosticReasons(t *testing.T) {
	for _, scenario := range []string{"migration_busy", "source_busy", "target_conflict", "entry_hash_mismatch", "stage_owner_mismatch", "invalid_record"} {
		t.Run(scenario, func(t *testing.T) {
			old, current, lib := legacyFixture(t, false)
			switch scenario {
			case "migration_busy":
				os.MkdirAll(current, 0700)
				lock := flock.New(filepath.Join(current, ".migration.lock"))
				ok, err := lock.TryLock()
				if err != nil || !ok {
					t.Fatal(ok, err)
				}
				defer lock.Unlock()
			case "source_busy":
				lock := flock.New(filepath.Join(lib, "reader.lock"))
				ok, err := lock.TryLock()
				if err != nil || !ok {
					t.Fatal(ok, err)
				}
				defer lock.Unlock()
			case "invalid_record":
				if err := os.MkdirAll(current, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(current, "migrations.json"), []byte("PRIVATE_RECORD_SENTINEL"), 0600); err != nil {
					t.Fatal(err)
				}
			case "target_conflict":
				state, err := MigrateReaderData(old, current)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(filepath.Join(current, "proxy-settings.json"), filepath.Join(current, migrationStage, "app", "proxy-settings.json")); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(current, "proxy-settings.json"), []byte("PRIVATE_NEW_SETTINGS"), 0600); err != nil {
					t.Fatal(err)
				}
				file, _, _ := readMigration(current)
				state.Status = "verified"
				if err = saveMigration(current, file, state); err != nil {
					t.Fatal(err)
				}
			case "entry_hash_mismatch", "stage_owner_mismatch":
				state, err := MigrateReaderData(old, current)
				if err != nil {
					t.Fatal(err)
				}
				file, _, _ := readMigration(current)
				state.Status = "verified"
				if err := saveMigration(current, file, state); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(current, "proxy-settings.json")
				if scenario == "stage_owner_mismatch" {
					path = filepath.Join(current, migrationStage, "owner")
				}
				if err := os.WriteFile(path, []byte("PRIVATE_CONTENT_SENTINEL"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
			defer slog.SetDefault(previous)
			if _, err := MigrateReaderData(old, current); err == nil {
				t.Fatal("accepted conflict or modified data")
			}
			log := output.String()
			if !strings.Contains(log, `"reasonCode":"`+scenario+`"`) {
				t.Fatal(log)
			}
			if strings.Contains(log, "PRIVATE_") || strings.Contains(log, old) || strings.Contains(log, current) {
				t.Fatal("private migration data in logs")
			}
		})
	}
}

func TestReaderMigrationPreservesCacheNamedCustomLibrary(t *testing.T) {
	for _, collision := range []bool{false, true} {
		t.Run(fmt.Sprintf("collision=%v", collision), func(t *testing.T) {
			old, current, lib := legacyFixture(t, false)
			custom := filepath.Join(old, "GraphiteDawnCache")
			if err := os.Rename(lib, custom); err != nil {
				t.Fatal(err)
			}
			if err := atomicMigrationJSON(filepath.Join(old, "library-location.json"), map[string]string{"directory": custom}); err != nil {
				t.Fatal(err)
			}
			if collision {
				writeMigrationCache(t, current, "GraphiteDawnCache", "live cache")
			}
			state, err := MigrateReaderData(old, current)
			if collision {
				var classified *migrationError
				if !errors.As(err, &classified) || classified.reason != "target_conflict" {
					t.Fatal(err)
				}
				if !pathExists(filepath.Join(custom, "reader.sqlite")) {
					t.Fatal("lost source library")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertMigrated(t, state.Library)
		})
	}
}

func TestReaderMigrationRejectsUnknownDigestVersion(t *testing.T) {
	old, current, _ := legacyFixture(t, false)
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	file, _, _ := readMigration(current)
	state.DigestVersion = 99
	if err := saveMigration(current, file, state); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateReaderData(old, current); err == nil {
		t.Fatal("accepted unknown hash policy")
	}
}

func TestReaderMigrationOnlyCopiesBusinessData(t *testing.T) {
	old, current, _ := legacyFixture(t, false)
	for _, name := range []string{"GrShaderCache", "ShaderCache", "FutureChromiumDirectory2099", "Local Storage", "user-backup"} {
		writeMigrationCache(t, old, name, "old unrelated data")
		writeMigrationCache(t, current, name, "new runtime data")
	}
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(state)
	var fields map[string]any
	json.Unmarshal(encoded, &fields)
	if fields["dataOnly"] != true {
		t.Fatal("new migration must use the business-data allowlist")
	}
	if len(state.Entries) != 3 {
		t.Fatal("unexpected activation entries")
	}
	assertMigrated(t, state.Library)
	for _, name := range []string{"GrShaderCache", "ShaderCache", "FutureChromiumDirectory2099", "Local Storage", "user-backup"} {
		if _, found := state.Entries[name]; found {
			t.Fatal("unrelated profile data in activation manifest")
		}
		b, err := os.ReadFile(filepath.Join(current, name, "cache"))
		if err != nil || string(b) != "new runtime data" {
			t.Fatal("changed live profile data")
		}
		if pathExists(filepath.Join(current, migrationStage, "app", name)) {
			t.Fatal("copied profile data into staging")
		}
		writeMigrationCache(t, old, name, "profile changed after migration")
	}
	if _, err := CleanupReaderData(old, current, "delete"); err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(old, "library", "reader.sqlite")) || pathExists(filepath.Join(old, "proxy-settings.json")) {
		t.Fatal("did not clean migrated data")
	}
	if !pathExists(filepath.Join(old, "user-backup", "cache")) {
		t.Fatal("deleted data outside the allowlist")
	}
}

func TestReaderMigrationResumesOldVerifiedProfileWithoutActivatingIt(t *testing.T) {
	old, current, _ := legacyFixture(t, false)
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(state)
	var fields map[string]json.RawMessage
	json.Unmarshal(encoded, &fields)
	delete(fields, "dataOnly")
	encoded, _ = json.Marshal(fields)
	state = new(ReaderMigration)
	if err := json.Unmarshal(encoded, state); err != nil {
		t.Fatal(err)
	}
	state.DigestVersion = 1
	for _, name := range []string{"GrShaderCache", "ShaderCache", "FutureChromiumDirectory2099"} {
		writeMigrationCache(t, old, name, "old profile")
		writeMigrationCache(t, filepath.Join(current, migrationStage, "app"), name, "old profile")
		writeMigrationCache(t, current, name, "active profile")
		state.Entries[name], _, err = treeDigest(filepath.Join(old, name))
		if err != nil {
			t.Fatal(err)
		}
	}
	state.SourceHash, _, err = state.treeDigest(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(current, "proxy-settings.json"), filepath.Join(current, migrationStage, "app", "proxy-settings.json")); err != nil {
		t.Fatal(err)
	}
	file, _, err := readMigration(current)
	if err != nil {
		t.Fatal(err)
	}
	state.Status, state.CompletedAt, state.LegacyCleanup = "verified", "", nil
	if err := saveMigration(current, file, state); err != nil {
		t.Fatal(err)
	}
	state, err = MigrateReaderData(old, current)
	if err != nil || state.Status != "completed" {
		t.Fatal(state, err)
	}
	assertMigrated(t, state.Library)
	for _, name := range []string{"GrShaderCache", "ShaderCache", "FutureChromiumDirectory2099"} {
		b, err := os.ReadFile(filepath.Join(current, name, "cache"))
		if err != nil || string(b) != "active profile" {
			t.Fatal("modified an old profile collision")
		}
	}
	if _, err := CleanupReaderData(old, current, "delete"); err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(old, "FutureChromiumDirectory2099")) {
		t.Fatal("cleaned unselected profile data")
	}
}

func TestReaderMigrationSelectsNestedLibraryWithoutItsSiblings(t *testing.T) {
	old, current, lib := legacyFixture(t, false)
	custom := filepath.Join(old, "custom", "reading")
	if err := os.MkdirAll(filepath.Dir(custom), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(lib, custom); err != nil {
		t.Fatal(err)
	}
	if err := atomicMigrationJSON(filepath.Join(old, "library-location.json"), map[string]string{"directory": custom}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "custom", "private-note.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(current, "custom"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, "custom", "runtime-state"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	assertMigrated(t, state.Library)
	if pathExists(filepath.Join(current, "custom", "private-note.txt")) {
		t.Fatal("copied an unselected sibling")
	}
	if !pathExists(filepath.Join(current, "custom", "runtime-state")) {
		t.Fatal("overwrote target sibling")
	}
	if _, err := CleanupReaderData(old, current, "delete"); err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(old, "custom", "private-note.txt")) {
		t.Fatal("deleted an unselected sibling")
	}
}

func TestReaderMigrationDoesNotTraverseUnselectedProfileLinks(t *testing.T) {
	old, current, _ := legacyFixture(t, false)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "private.txt"), []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(old, "UnknownProfileData")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip(err)
	}
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(current, "UnknownProfileData")) {
		t.Fatal("copied an unselected profile link")
	}
	if _, err = CleanupReaderData(old, current, "delete"); err != nil {
		t.Fatal(err)
	}
	if !pathExists(link) || !pathExists(filepath.Join(outside, "private.txt")) {
		t.Fatal("changed data outside selected paths")
	}
	assertMigrated(t, state.Library)
}

func TestReaderMigrationRestartsOldCopyUsingBusinessSelection(t *testing.T) {
	old, current, lib := legacyFixture(t, false)
	database := filepath.Join(lib, "reader.sqlite")
	original, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(database, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = MigrateReaderData(old, current); err == nil {
		t.Fatal("accepted broken database")
	}
	file, state, err := readMigration(current)
	if err != nil {
		t.Fatal(err)
	}
	state.DataOnly = false
	state.DigestVersion = 1
	if err = saveMigration(current, file, state); err != nil {
		t.Fatal(err)
	}
	writeMigrationCache(t, filepath.Join(current, migrationStage, "app"), "GrShaderCache", "old unfinished copy")
	writeMigrationCache(t, old, "GrShaderCache", "old cache")
	writeMigrationCache(t, current, "GrShaderCache", "live cache")
	if err = os.WriteFile(database, original, 0600); err != nil {
		t.Fatal(err)
	}
	state, err = MigrateReaderData(old, current)
	if err != nil || !state.DataOnly {
		t.Fatal(state, err)
	}
	if pathExists(filepath.Join(current, migrationStage, "app", "GrShaderCache")) {
		t.Fatal("kept copying unselected profile data")
	}
	assertMigrated(t, state.Library)
}

func TestReaderMigrationCopiesWholeExternalBusinessLibrary(t *testing.T) {
	old, current, lib := legacyFixture(t, true)
	writeMigrationCache(t, lib, "GrShaderCache", "data inside the selected library")
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(state.Library, "GrShaderCache", "cache"))
	if err != nil || string(data) != "data inside the selected library" {
		t.Fatal("applied a browser cache blacklist inside the selected library")
	}
}

func TestReaderCleanupPreservesInterruptedQuarantine(t *testing.T) {
	old, current, _ := legacyFixture(t, false)
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(filepath.Dir(old), ".reader-cleanup-"+state.ID)
	if err := os.Rename(old, quarantine); err != nil {
		t.Fatal(err)
	}
	_, err = CleanupReaderData(old, current, "delete")
	if err == nil || migrationFailureReason(err) != "cleanup_interrupted" {
		t.Fatal("must report quarantined data even when the old directory is missing", err)
	}
	if !pathExists(filepath.Join(quarantine, "library", "reader.sqlite")) {
		t.Fatal("deleted interrupted cleanup data")
	}
	_, saved, err := readMigration(current)
	if err != nil || saved.LegacyCleanup.Status != "failed" {
		t.Fatal("incorrectly marked interrupted cleanup complete", err)
	}
}

// This must include normal service initialization between migration and cleanup.
// Calling the two migration functions back-to-back misses citation's writes.
func cleanupWithCitation(t *testing.T, previousFiles bool, legacy ...bool) (string, string, *Server) {
	t.Helper()
	old, current, _ := legacyFixture(t, false)
	if previousFiles {
		for name, value := range map[string]string{"cite-credential": strings.Repeat("1", 64), "cite-wps-key": strings.Repeat("2", 64)} {
			if err := os.WriteFile(filepath.Join(old, name), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	state, err := MigrateReaderData(old, current)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) > 0 && legacy[0] {
		state.DataOnly = false
		state.SourceHash, _, err = state.treeDigest(old)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range readerCitationNames {
			if !pathExists(filepath.Join(old, name)) {
				continue
			}
			data, err := os.ReadFile(filepath.Join(old, name))
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(current, name), data, 0600); err != nil {
				t.Fatal(err)
			}
			state.Entries[name], err = digestEntry(filepath.Join(current, name))
			if err != nil {
				t.Fatal(err)
			}
		}
		file, _, err := readMigration(current)
		if err != nil {
			t.Fatal(err)
		}
		if err = saveMigration(current, file, state); err != nil {
			t.Fatal(err)
		}
	}
	store, err := OpenLibrary(state.Library)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DB.Close() })
	server := NewServer(store, "fixture-auth", "")
	t.Cleanup(server.Close)
	if err := server.SetCiteCredential(filepath.Join(old, "cite-credential")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "cite-wps-key"), []byte(strings.Repeat("3", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	return old, current, server
}

func TestReaderCleanupPreservesLiveCitationFiles(t *testing.T) {
	for _, scenario := range []string{"new_files", "rotated_files", "unversioned_record", "legacy_rotated"} {
		t.Run(scenario, func(t *testing.T) {
			old, current, server := cleanupWithCitation(t, scenario != "new_files", scenario == "unversioned_record" || scenario == "legacy_rotated")
			if scenario == "unversioned_record" {
				file, state, err := readMigration(current)
				if err != nil {
					t.Fatal(err)
				}
				state.DigestVersion = 0
				if err := saveMigration(current, file, state); err != nil {
					t.Fatal(err)
				}
			}
			_, before, err := readMigration(current)
			if err != nil {
				t.Fatal(err)
			}
			credential, err := os.ReadFile(filepath.Join(old, "cite-credential"))
			if err != nil {
				t.Fatal(err)
			}
			state, err := CleanupReaderData(old, current, "delete")
			if err != nil {
				t.Fatal(err)
			}
			if state.SourceHash != before.SourceHash || state.DigestVersion != before.DigestVersion {
				t.Fatal("rewrote the historical source fingerprint")
			}
			if state.LegacyCleanup.Status != "deleted" {
				t.Fatal(state.LegacyCleanup.Status)
			}
			if pathExists(filepath.Join(old, "library", "reader.sqlite")) || pathExists(filepath.Join(old, "proxy-settings.json")) {
				t.Fatal("did not remove migrated reading data")
			}
			assertMigrated(t, state.Library)
			preserved, err := os.ReadFile(filepath.Join(old, "cite-credential"))
			if err != nil || string(preserved) != string(credential) {
				t.Fatal("Word credential lost or changed")
			}
			request := httptest.NewRequest("POST", "http://127.0.0.1:17841/v1/cite/bootstrap", nil)
			request.Header.Set("Authorization", "Bearer "+strings.Repeat("3", 64))
			response := httptest.NewRecorder()
			server.citeBootstrap(response, request)
			if response.Code != 200 {
				t.Fatalf("WPS bootstrap broke after cleanup: %d", response.Code)
			}
			if _, err := MigrateReaderData(old, current); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReaderCleanupStillRejectsOtherChanges(t *testing.T) {
	for _, scenario := range []string{"document_change", "unexpected_file", "invalid_credential", "credential_symlink", "tampered_snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			old, current, _ := cleanupWithCitation(t, scenario == "tampered_snapshot", scenario == "invalid_credential" || scenario == "credential_symlink" || scenario == "tampered_snapshot")
			var err error
			switch scenario {
			case "document_change":
				err = os.WriteFile(filepath.Join(old, "library", "papers", "paper.pdf"), []byte("PRIVATE_CHANGED_DOCUMENT"), 0600)
			case "unexpected_file":
				err = os.WriteFile(filepath.Join(old, "library", "PRIVATE_NEW_NOTE"), []byte("new note"), 0600)
			case "invalid_credential":
				err = os.WriteFile(filepath.Join(old, "cite-credential"), []byte("PRIVATE_NON_CREDENTIAL"), 0600)
			case "credential_symlink":
				if os.PathSeparator != '/' {
					t.Skip("symlink privilege depends on Windows host")
				}
				if err = os.Remove(filepath.Join(old, "cite-credential")); err == nil {
					err = os.Symlink(filepath.Join(old, "cite-wps-key"), filepath.Join(old, "cite-credential"))
				}
			case "tampered_snapshot":
				err = os.WriteFile(filepath.Join(current, "cite-credential"), []byte(strings.Repeat("4", 64)), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := CleanupReaderData(old, current, "delete"); err == nil {
				t.Fatal("deleted changed or unverifiable reading data")
			}
			if _, err := RetryReaderCleanup(old, current); err == nil {
				t.Fatal("retry bypassed source verification")
			}
			if !pathExists(filepath.Join(old, "library", "reader.sqlite")) {
				t.Fatal("lost original library")
			}
			assertMigrated(t, filepath.Join(current, "library"))
		})
	}
}

func TestReaderCleanupDiagnosticPrivacy(t *testing.T) {
	old, current, _ := cleanupWithCitation(t, false)
	if err := os.WriteFile(filepath.Join(old, "library", "PRIVATE_NEW_NOTE"), []byte("PRIVATE_DOCUMENT_SENTINEL"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	if _, err := CleanupReaderData(old, current, "delete"); err == nil {
		t.Fatal("accepted changed source")
	}
	log := output.String()
	if !strings.Contains(log, `"msg":"migration.cleanup_failed","phase":"verify_source","reasonCode":"source_changed"`) {
		t.Fatal("missing controlled cleanup failure reason")
	}
	if strings.Contains(log, "PRIVATE_") || strings.Contains(log, old) || strings.Contains(log, strings.Repeat("3", 64)) {
		t.Fatal("private content in cleanup diagnostics")
	}
}

func TestReaderCleanupRuntimeSnapshotAndHashFormat(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "a"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{filepath.Join("a", "one"): "a", "z": "z", "reader.lock": "excluded"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	hash, _, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	expected := "f9211490286efa52cfe5fd49b37cc9e9bbdfd0d4771fd99e56a2c2e62af80714"
	if os.PathSeparator == '\\' {
		expected = "a330b681af36ae8dfd3bf91718066ee10392a19cd16982577d38549a94ccc3ab"
	}
	if hash != expected {
		t.Fatal("changed historical source hash format")
	}
}

func TestReaderCleanupKeepsDataWhenQuarantineExists(t *testing.T) {
	old, current, _ := cleanupWithCitation(t, false)
	_, state, err := readMigration(current)
	if err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(filepath.Dir(old), ".reader-cleanup-"+state.ID)
	if err := os.Mkdir(quarantine, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := CleanupReaderData(old, current, "delete"); err == nil {
		t.Fatal("overwrote existing cleanup directory")
	}
	if !pathExists(filepath.Join(old, "library", "reader.sqlite")) {
		t.Fatal("lost old library")
	}
	if !pathExists(filepath.Join(old, "cite-credential")) {
		t.Fatal("lost live Word credential")
	}
}

func TestReaderCleanupRetryIsExplicit(t *testing.T) {
	for _, status := range []string{"failed", "kept"} {
		t.Run(status, func(t *testing.T) {
			old, current, _ := cleanupWithCitation(t, false)
			file, state, err := readMigration(current)
			if err != nil {
				t.Fatal(err)
			}
			state.LegacyCleanup.Status = status
			if err := saveMigration(current, file, state); err != nil {
				t.Fatal(err)
			}
			if _, err := CleanupReaderData(old, current, "delete"); err != nil {
				t.Fatal(err)
			}
			if !pathExists(filepath.Join(old, "library", "reader.sqlite")) {
				t.Fatal("automatically retried cleanup")
			}
			result, err := RetryReaderCleanup(old, current)
			if status == "kept" {
				if err == nil || !pathExists(filepath.Join(old, "library", "reader.sqlite")) {
					t.Fatal("overrode decision to keep data")
				}
			} else {
				if err != nil || result.LegacyCleanup.Status != "deleted" {
					t.Fatal("explicit retry failed", err)
				}
				if pathExists(filepath.Join(old, "library", "reader.sqlite")) || !pathExists(filepath.Join(old, "cite-credential")) {
					t.Fatal("retry removed the wrong data")
				}
			}
		})
	}
}

func TestReaderCleanupRetryDoesNotReplaceInitialChoice(t *testing.T) {
	old, current, _ := cleanupWithCitation(t, false)
	if _, err := RetryReaderCleanup(old, current); err == nil {
		t.Fatal("used retry before initial cleanup decision")
	}
	if !pathExists(filepath.Join(old, "library", "reader.sqlite")) {
		t.Fatal("deleted data before initial decision")
	}
	_, state, err := readMigration(current)
	if err != nil || state.LegacyCleanup.Status != "pending" {
		t.Fatal("changed initial cleanup decision")
	}
}
