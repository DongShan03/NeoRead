package neoread

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func syncDocumentFixture(t *testing.T, store *Store, id, folder string) {
	t.Helper()
	archiveDocument(t, store, id, folder)
	if err := os.WriteFile(filepath.Join(store.Root, "cache", id, "analysis", "manifest.json"), []byte(`{"pages":1,"blocks":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
}

func syncFixture(t *testing.T, remote *httptest.Server, name string) *Server {
	t.Helper()
	store := archiveFixture(t, t.TempDir())
	server := NewServer(store, "test-token", "")
	server.webdav.factory = func(c webdavConfig) *davClient { return testDAVClient(c, remote) }
	server.webdav.beforeApply = func(context.Context) error { return nil }
	config := webdavConfig{Account: "reader@example.com", Password: "application-password", Folder: "论文库", DeviceName: name, IntervalMinutes: 15, Enabled: true}
	if err := server.writeSyncConfig(config); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return server
}
func TestWebDAVDoesNotAcknowledgeEditsThatArriveDuringDownload(t *testing.T) {
	remote, _ := testDAV(t)
	a := syncFixture(t, remote, "Mac")
	b := syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	runSync(t, a)
	runSync(t, b)
	if _, err := a.Store.DB.Exec("UPDATE document_notes SET body='cloud change' WHERE document_id='paper'"); err != nil {
		t.Fatal(err)
	}
	runSync(t, a)
	b.webdav.beforeApply = func(context.Context) error {
		_, err := b.Store.DB.Exec("UPDATE document_notes SET body='edit while downloading' WHERE document_id='paper'")
		return err
	}
	runSync(t, b)
	var note string
	if err := b.Store.DB.QueryRow("SELECT body FROM document_notes WHERE document_id='paper'").Scan(&note); err != nil || note != "edit while downloading" {
		t.Fatal(note, err)
	}
	b.webdav.beforeApply = func(context.Context) error { return nil }
	runSync(t, b)
	if len(b.syncStatus().Conflicts) != 1 {
		t.Fatal("unapplied cloud changes were incorrectly acknowledged", b.syncStatus())
	}
}

type davTestTransport func(*http.Request) (*http.Response, error)

func (f davTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestWebDAVDeletedDocumentIsNotResurrectedAfterFailedFirstUpload(t *testing.T) {
	remote, _ := testDAV(t)
	a := syncFixture(t, remote, "Mac")
	b := syncFixture(t, remote, "Windows")
	runSync(t, a)
	syncDocumentFixture(t, a.Store, "paper", "分类")
	a.webdav.factory = func(c webdavConfig) *davClient {
		client := testDAVClient(c, remote)
		client.http.Transport = davTestTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method == "PUT" && strings.Contains(r.URL.Path, "/devices/") {
				return &http.Response{StatusCode: 503, Header: http.Header{}, Body: http.NoBody}, nil
			}
			return http.DefaultTransport.RoundTrip(r)
		})
		return client
	}
	config, _ := a.readSyncConfig()
	if err := a.runWebDAV(context.Background(), config); err == nil {
		t.Fatal("expected upload failure")
	}
	if _, err := a.Store.DB.Exec("DELETE FROM documents WHERE id='paper'"); err != nil {
		t.Fatal(err)
	}
	a.webdav.factory = func(c webdavConfig) *davClient { return testDAVClient(c, remote) }
	runSync(t, a)
	runSync(t, b)
	if _, err := b.Store.Document("paper"); err == nil {
		t.Fatal("deleted, unpublished document was resurrected")
	}
}

func TestWebDAVBusyDocumentDoesNotDeleteItsPendingFirstUpload(t *testing.T) {
	remote, _ := testDAV(t)
	a := syncFixture(t, remote, "Mac")
	b := syncFixture(t, remote, "Windows")
	runSync(t, a)
	syncDocumentFixture(t, a.Store, "paper", "分类")
	config, _ := a.readSyncConfig()
	state, err := a.loadSyncState(config)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := a.snapshotSync(t.Context(), state)
	if err != nil {
		t.Fatal(err)
	}
	key := "document/paper"
	state.Own[key] = newSyncVersion(state, snapshot.Documents[key], nil, "Mac")
	if err = a.saveSyncState(config, state); err != nil {
		t.Fatal(err)
	}
	_, finish, err := a.beginDocumentTask(t.Context(), "paper")
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	runSync(t, a)
	state, err = a.loadSyncState(config)
	if err != nil || state.Own[key].Deleted {
		t.Fatal("busy document became a deletion", err)
	}
	runSync(t, b)
	if _, err = b.Store.Document("paper"); err != nil {
		t.Fatal("pending stable version was lost", err)
	}
}

func waitSyncPhase(t *testing.T, s *Server, phase string) webdavStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status := s.syncStatus()
		if status.Phase == phase {
			return status
		}
		if status.Phase == "failed" {
			t.Fatal(status.Error)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("did not reach %s: %+v", phase, s.syncStatus())
	return webdavStatus{}
}
func TestWebDAVWaitsForRendererSaveBeforeApplyingIncomingContent(t *testing.T) {
	remote, _ := testDAV(t)
	a := syncFixture(t, remote, "Mac")
	b := syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	runSync(t, a)
	b.webdav.beforeApply = nil
	if err := b.beginWebDAVSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := waitSyncPhase(t, b, "ready")
	if _, err := b.Store.Document("paper"); err == nil {
		t.Fatal("applied before renderer saved")
	}
	body, _ := json.Marshal(map[string]string{"token": status.ApplyToken})
	request := httptest.NewRequest("POST", "http://127.0.0.1/api/sync/webdav/apply", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	b.Handler().ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var applied webdavStatus
	if err := json.Unmarshal(response.Body.Bytes(), &applied); err != nil || applied.Revision == 0 {
		t.Fatal("apply did not publish its revision", response.Body.String(), err)
	}
	if _, err := b.Store.Document("paper"); err != nil {
		t.Fatal(err)
	}
	b.webdav.mu.Lock()
	done := b.webdav.done
	b.webdav.mu.Unlock()
	<-done
}
func TestWebDAVRecoversFileInstallationOnEitherSideOfCommit(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "commit"}[committed], func(t *testing.T) {
			s := NewServer(archiveFixture(t, t.TempDir()), "token", "")
			run := id()
			name := "papers/paper.pdf"
			backup := filepath.Join(s.syncDirectory(), "apply", run, "backup", name)
			if err := os.MkdirAll(filepath.Dir(backup), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(backup, []byte("previous original"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(s.Store.Root, name), []byte("incoming original"), 0600); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(syncInstallJournal{ID: run, Files: []syncInstallFile{{Name: name, Existed: true}}})
			if err := atomicFile(filepath.Join(s.syncDirectory(), "apply.json"), body); err != nil {
				t.Fatal(err)
			}
			if committed {
				if _, err := s.Store.DB.Exec("INSERT INTO settings(key,value) VALUES('sync:apply',?)", run); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.recoverSyncInstall(); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(s.Store.Root, name))
			expected := "previous original"
			if committed {
				expected = "incoming original"
			}
			if err != nil || string(got) != expected {
				t.Fatal(string(got), err)
			}
			if _, err = os.Stat(filepath.Join(s.syncDirectory(), "apply.json")); !os.IsNotExist(err) {
				t.Fatal("recovery journal not cleared", err)
			}
		})
	}
}
func TestWebDAVSanitizesSyncedMarkupBeforePublishingToReaders(t *testing.T) {
	remote, _ := testDAV(t)
	a := syncFixture(t, remote, "Mac")
	b := syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	name := filepath.Join(a.Store.Root, "cache", "paper", "chapter.xhtml")
	if err := os.WriteFile(name, []byte(`<html><body><p onclick="evil()">read</p><script>steal()</script></body></html>`), 0600); err != nil {
		t.Fatal(err)
	}
	runSync(t, a)
	runSync(t, b)
	body, err := os.ReadFile(filepath.Join(b.Store.Root, "cache", "paper", "chapter.xhtml"))
	if err != nil || strings.Contains(string(body), "<script") || strings.Contains(string(body), "onclick") || !strings.Contains(string(body), "read") {
		t.Fatal(string(body), err)
	}
}
func TestWebDAVReadingMergeUsesActualTimestampsAndKeepsPeakPosition(t *testing.T) {
	a := syncRow{"id": "a", "created_at": "2026-11-03T10:00:00Z", "last_opened_at": "2026-11-03T10:00:00Z", "progress": "older", "body_read_position": 9.0}
	b := syncRow{"id": "a", "created_at": "2026-11-02T10:00:00Z", "last_opened_at": "2026-11-03T10:00:00.2Z", "progress": "newer", "body_read_position": 3.0}
	got, ok := mergeSyncDocumentRow(nil, []syncRow{a}, []syncRow{b})
	if !ok || got["created_at"] != b["created_at"] || got["progress"] != "newer" || syncNumber(got["body_read_position"]) != 9 {
		t.Fatal(got, ok)
	}
}

func TestWebDAVSameNoteTextWithDifferentSaveTimesDoesNotConflict(t *testing.T) {
	a, b := emptySyncDocument(), emptySyncDocument()
	key := `document_notes/["paper"]`
	a.Records[key] = syncRecord{"document_notes": {{"document_id": "paper", "body": "same note", "updated_at": "2026-11-03T10:00:00Z"}}}
	b.Records[key] = syncRecord{"document_notes": {{"document_id": "paper", "body": "same note", "updated_at": "2026-11-03T10:01:00Z"}}}
	merged, ok := mergeSyncDocument(nil, a, b)
	if !ok || merged.Records[key]["document_notes"][0]["updated_at"] != "2026-11-03T10:01:00Z" {
		t.Fatal("identical notes caused a conflict", merged, ok)
	}
}
func runSync(t *testing.T, s *Server) {
	t.Helper()
	config, err := s.readSyncConfig()
	if err == nil {
		err = s.runWebDAV(context.Background(), config)
	}
	if err != nil {
		t.Fatal(err)
	}
}
func TestWebDAVRoundTripKeepsOriginalNotesChatsAndOtherLibraryModes(t *testing.T) {
	remote, _ := testDAV(t)
	a := syncFixture(t, remote, "Mac")
	b := syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	syncDocumentFixture(t, a.Store, "book", "图书分类")
	if _, err := a.Store.DB.Exec("UPDATE documents SET library='books' WHERE id='book'"); err != nil {
		t.Fatal(err)
	}
	secret := "not-a-synced-ai-key"
	if err := os.WriteFile(filepath.Join(a.Store.Root, "ai-connections.json"), []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	runSync(t, a)
	runSync(t, b)
	for _, doc := range []string{"paper", "book"} {
		if _, err := b.Store.Document(doc); err != nil {
			t.Fatalf("missing document %s: %v", doc, err)
		}
		var note, chat string
		if err := b.Store.DB.QueryRow("SELECT body FROM document_notes WHERE document_id=?", doc).Scan(&note); err != nil || !strings.Contains(note, "原样保存") {
			t.Fatalf("note: %s %v", note, err)
		}
		if err := b.Store.DB.QueryRow("SELECT body FROM messages WHERE document_id=?", doc).Scan(&chat); err != nil || !strings.Contains(chat, "回答") {
			t.Fatalf("chat: %s %v", chat, err)
		}
		body, err := os.ReadFile(filepath.Join(b.Store.Root, "papers", doc+".pdf"))
		if err != nil || string(body) != "PDF "+doc {
			t.Fatalf("original: %s %v", body, err)
		}
		processing, err := b.Store.processing(doc)
		if err != nil || processing.Phase == "learning" || processing.Status != "complete" {
			t.Fatalf("synced document cannot open immediately: %+v %v", processing, err)
		}
	}
	if _, err := os.Stat(filepath.Join(b.Store.Root, "ai-connections.json")); !os.IsNotExist(err) {
		t.Fatal("machine credentials were synced")
	}
	// A subsequent unchanged sync should preserve local state and files.
	runSync(t, a)
	runSync(t, b)
}

func TestWebDAVRebuildsOverviewSearchAndKeepsMachinePreferences(t *testing.T) {
	remote, _ := testDAV(t)
	a := syncFixture(t, remote, "Mac")
	b := syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	if _, err := a.Store.DB.Exec(`INSERT INTO document_overviews(document_id,body) VALUES('paper','{"corpusRevision":"revision","content":{"summary":"syncsearchterm","terms":["needle"]}}'); INSERT INTO settings(key,value) VALUES('preferences:library','{"papers":{"categories":["空分类"]},"mode":"papers"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.DB.Exec(`INSERT INTO settings(key,value) VALUES('preferences:library','{"mode":"books","primaryMode":"books"}')`); err != nil {
		t.Fatal(err)
	}
	runSync(t, a)
	runSync(t, b)
	var count int
	if err := b.Store.DB.QueryRow("SELECT count(*) FROM document_overviews_fts WHERE document_overviews_fts MATCH 'syncsearchterm'").Scan(&count); err != nil || count != 1 {
		t.Fatal("overview search not rebuilt", count, err)
	}
	var preferences string
	if err := b.Store.DB.QueryRow("SELECT value FROM settings WHERE key='preferences:library'").Scan(&preferences); err != nil || !strings.Contains(preferences, "空分类") || !strings.Contains(preferences, `"mode":"books"`) {
		t.Fatal(preferences, err)
	}
}

func TestWebDAVRestoresLastSyncAfterRestart(t *testing.T) {
	remote, _ := testDAV(t)
	s := syncFixture(t, remote, "Mac")
	runSync(t, s)
	last := s.syncStatus().LastSync
	s.webdav.status = webdavStatus{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartWebDAV(ctx)
	if s.syncStatus().LastSync != last || s.webdav.next.Before(time.Now()) {
		t.Fatal("last sync schedule not restored", s.syncStatus())
	}
}

func TestWebDAVRenameCanRevertAPendingLocalRename(t *testing.T) {
	remote, _ := testDAV(t)
	s := syncFixture(t, remote, "Mac")
	runSync(t, s)
	for _, name := range []string{"临时名称", "论文库"} {
		body, _ := json.Marshal(map[string]string{"name": name})
		r := httptest.NewRequest("POST", "http://127.0.0.1/api/sync/webdav/rename", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	runSync(t, s)
	config, _ := s.readSyncConfig()
	if config.Folder != "论文库" {
		t.Fatal("rolled-back rename was sent to cloud", config.Folder)
	}
}
func TestWebDAVMergesIndependentEditsAndPreservesNoteConflicts(t *testing.T) {
	remote, _ := testDAV(t)
	a := syncFixture(t, remote, "Mac")
	b := syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	runSync(t, a)
	runSync(t, b)
	if _, err := a.Store.DB.Exec("UPDATE document_notes SET body='Mac 修改' WHERE document_id='paper'"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.DB.Exec("UPDATE documents SET title='Windows 标题' WHERE id='paper'"); err != nil {
		t.Fatal(err)
	}
	runSync(t, a)
	runSync(t, b)
	runSync(t, a)
	var note string
	if err := a.Store.DB.QueryRow("SELECT body FROM document_notes WHERE document_id='paper'").Scan(&note); err != nil || note != "Mac 修改" {
		t.Fatal(note, err)
	}
	doc, err := a.Store.Document("paper")
	if err != nil || doc.Title != "Windows 标题" {
		t.Fatal(doc, err)
	}
	if _, err := a.Store.DB.Exec("UPDATE document_notes SET body='版本 A' WHERE document_id='paper'"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.DB.Exec("UPDATE document_notes SET body='版本 B' WHERE document_id='paper'"); err != nil {
		t.Fatal(err)
	}
	runSync(t, a)
	runSync(t, b)
	status := b.syncStatus()
	if len(status.Conflicts) != 1 || len(status.Conflicts[0].Versions) != 2 {
		t.Fatalf("conflicts: %+v", status)
	}
	if err := b.Store.DB.QueryRow("SELECT body FROM document_notes WHERE document_id='paper'").Scan(&note); err != nil || note != "版本 B" {
		t.Fatal("conflict overwrote local note", note, err)
	}
	conflict := status.Conflicts[0]
	var selected webdavVariant
	for _, version := range conflict.Versions {
		if strings.Contains(version.Preview, "版本 A") {
			selected = version
		}
	}
	if selected.ID == "" {
		t.Fatal("missing conflict preview")
	}
	config, _ := b.readSyncConfig()
	state, err := b.loadSyncState(config)
	if err != nil {
		t.Fatal(err)
	}
	state.Choices[conflict.Key] = syncChoice{Conflict: conflict.ID, Version: selected.ID}
	if err = b.saveSyncState(config, state); err != nil {
		t.Fatal(err)
	}
	runSync(t, b)
	runSync(t, a)
	if len(b.syncStatus().Conflicts) != 0 {
		t.Fatal("conflict not resolved")
	}
	if err = b.Store.DB.QueryRow("SELECT body FROM document_notes WHERE document_id='paper'").Scan(&note); err != nil || note != "版本 A" {
		t.Fatal(note, err)
	}
}

func TestWebDAVKeepBothNotesResolvesConflictForAllDevices(t *testing.T) {
	remote, _ := testDAV(t)
	a, b := syncFixture(t, remote, "Mac"), syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	runSync(t, a)
	runSync(t, b)
	if _, err := a.Store.DB.Exec("UPDATE document_notes SET body='Mac 完整笔记' WHERE document_id='paper'"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.DB.Exec("UPDATE document_notes SET body='Windows 完整笔记' WHERE document_id='paper'"); err != nil {
		t.Fatal(err)
	}
	runSync(t, a)
	runSync(t, b)
	conflict := b.syncStatus().Conflicts[0]
	if !conflict.CanKeepBoth {
		t.Fatal("note conflict cannot keep both")
	}
	body := jsonText(map[string]any{"key": conflict.Key, "conflict": conflict.ID, "keepBoth": true})
	r := httptest.NewRequest("POST", "http://127.0.0.1/api/sync/webdav/resolve", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	b.Handler().ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(b.syncStatus().Conflicts) != 0 {
		t.Fatal("resolved conflict still prompts")
	}
	b.webdav.mu.Lock()
	done := b.webdav.done
	b.webdav.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resolution did not finish")
	}
	for i := 0; i < 2; i++ {
		runSync(t, a)
		runSync(t, b)
	}
	for _, server := range []*Server{a, b} {
		var note string
		if err := server.Store.DB.QueryRow("SELECT body FROM document_notes WHERE document_id='paper'").Scan(&note); err != nil {
			t.Fatal(err)
		}
		if strings.Count(note, "Mac 完整笔记") != 1 || strings.Count(note, "Windows 完整笔记") != 1 || !strings.Contains(note, "## Mac") || !strings.Contains(note, "## Windows") {
			t.Fatal(note)
		}
		if len(server.syncStatus().Conflicts) != 0 || server.syncStatus().Dirty {
			t.Fatal("kept notes were not acknowledged", server.syncStatus())
		}
	}
}

func TestWebDAVTracksDirtyChangesAndLatchesFailuresUntilSuccess(t *testing.T) {
	remote, _ := testDAV(t)
	s := syncFixture(t, remote, "Mac")
	syncDocumentFixture(t, s.Store, "paper", "分类")
	runSync(t, s)
	if s.syncStatus().Dirty || s.syncStatus().Failed {
		t.Fatal("fresh sync is not clean", s.syncStatus())
	}
	if _, err := s.Store.DB.Exec("INSERT INTO settings(key,value) VALUES('api:key','private'); INSERT INTO settings(key,value) VALUES('preferences:library','{\"mode\":\"books\"}')"); err != nil {
		t.Fatal(err)
	}
	if s.syncStatus().Dirty {
		t.Fatal("machine configuration dirtied sync")
	}
	if _, err := s.Store.DB.Exec("UPDATE document_notes SET body='new edit' WHERE document_id='paper'"); err != nil {
		t.Fatal(err)
	}
	if !s.syncStatus().Dirty {
		t.Fatal("note edit was not detected")
	}
	s.webdav.factory = func(c webdavConfig) *davClient {
		client := testDAVClient(c, remote)
		client.http.Transport = davTestTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 500, Header: http.Header{}, Body: http.NoBody}, nil
		})
		return client
	}
	if err := s.beginWebDAVSync(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	s.webdav.mu.Lock()
	done := s.webdav.done
	s.webdav.mu.Unlock()
	<-done
	if !s.syncStatus().Failed || !s.syncStatus().Manual || s.syncStatus().RunID == "" {
		t.Fatal(s.syncStatus())
	}
	if _, err := s.Store.DB.Exec("UPDATE document_notes SET body='another edit' WHERE document_id='paper'"); err != nil {
		t.Fatal(err)
	}
	s.webdav.mu.Lock()
	s.webdav.status = webdavStatus{}
	s.webdav.mu.Unlock()
	if !s.syncStatus().Failed {
		t.Fatal("editing or restarting cleared failure")
	}
	s.webdav.factory = func(c webdavConfig) *davClient { return testDAVClient(c, remote) }
	runSync(t, s)
	if s.syncStatus().Failed || s.syncStatus().Dirty {
		t.Fatal("successful retry did not clear failure", s.syncStatus())
	}
}

func TestWebDAVChangesDuringUploadRemainUnsynced(t *testing.T) {
	remote, _ := testDAV(t)
	s := syncFixture(t, remote, "Mac")
	syncDocumentFixture(t, s.Store, "paper", "分类")
	changed := false
	s.webdav.factory = func(c webdavConfig) *davClient {
		client := testDAVClient(c, remote)
		client.http.Transport = davTestTransport(func(r *http.Request) (*http.Response, error) {
			if !changed && r.Method == "PUT" && strings.Contains(r.URL.Path, "/devices/") {
				changed = true
				if _, err := s.Store.DB.Exec("UPDATE document_notes SET body='edit while uploading' WHERE document_id='paper'"); err != nil {
					return nil, err
				}
			}
			return http.DefaultTransport.RoundTrip(r)
		})
		return client
	}
	runSync(t, s)
	if !s.syncStatus().Dirty {
		t.Fatal("late local edit was marked synced")
	}
	s.webdav.factory = func(c webdavConfig) *davClient { return testDAVClient(c, remote) }
	runSync(t, s)
	if s.syncStatus().Dirty {
		t.Fatal("follow-up sync did not acknowledge late edit")
	}
}

func TestWebDAVConfiguredConnectionIsAvailableButOffInOtherLibraries(t *testing.T) {
	remote, _ := testDAV(t)
	a, b := syncFixture(t, remote, "Mac"), NewServer(archiveFixture(t, t.TempDir()), "test-token", "")
	t.Cleanup(b.Close)
	shared := filepath.Join(t.TempDir(), "webdav-connection.json")
	if err := a.SetWebDAVAccountPath(shared); err != nil {
		t.Fatal(err)
	}
	if err := b.SetWebDAVAccountPath(shared); err != nil {
		t.Fatal(err)
	}
	config, err := b.readSyncConfig()
	if err != nil || config.Account != "reader@example.com" || !config.PasswordSet || config.Enabled || config.LibraryConfigured {
		t.Fatal(publicSyncConfig(config), err)
	}
	if err := b.beginWebDAVSync(t.Context(), true); err == nil {
		t.Fatal("unconfigured library could sync the default folder")
	}
}
func TestWebDAVRemoteFolderFollowsRenameAndOtherDevicesDiscoverIt(t *testing.T) {
	remote, directory := testDAV(t)
	a := syncFixture(t, remote, "Mac")
	b := syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	runSync(t, a)
	runSync(t, b)
	before, _ := a.readSyncConfig()
	oldState, _ := a.loadSyncState(before)
	if err := atomicFile(a.syncNamePath(), []byte("研究论文")); err != nil {
		t.Fatal(err)
	}
	runSync(t, a)
	runSync(t, b)
	for _, server := range []*Server{a, b} {
		config, _ := server.readSyncConfig()
		if config.Folder != "研究论文" || config.RemoteID != before.RemoteID {
			t.Fatalf("location changed identity: %+v", publicSyncConfig(config))
		}
	}
	after, _ := a.readSyncConfig()
	newState, _ := a.loadSyncState(after)
	if oldState.Device != newState.Device {
		t.Fatal("rename reset device identity")
	}
	if _, err := os.Stat(filepath.Join(directory, "NeoRead", "论文库")); !os.IsNotExist(err) {
		t.Fatal("old folder was recreated", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "NeoRead", "研究论文", "devices")); err != nil {
		t.Fatal(err)
	}
}
func TestWebDAVRenameNeverOverwritesAnotherFolder(t *testing.T) {
	remote, directory := testDAV(t)
	s := syncFixture(t, remote, "Mac")
	syncDocumentFixture(t, s.Store, "paper", "分类")
	runSync(t, s)
	target := filepath.Join(directory, "NeoRead", "已存在")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := atomicFile(s.syncNamePath(), []byte("已存在")); err != nil {
		t.Fatal(err)
	}
	config, _ := s.readSyncConfig()
	if err := s.runWebDAV(context.Background(), config); err == nil {
		t.Fatal("rename overwrote existing folder")
	}
	body, err := os.ReadFile(filepath.Join(target, "keep.txt"))
	if err != nil || string(body) != "keep" {
		t.Fatal(body, err)
	}
}

func TestWebDAVReusingARenamedLibraryNameCreatesAnIndependentLibrary(t *testing.T) {
	remote, _ := testDAV(t)
	a := syncFixture(t, remote, "Mac")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	runSync(t, a)
	if err := atomicFile(a.syncNamePath(), []byte("研究论文")); err != nil {
		t.Fatal(err)
	}
	runSync(t, a)
	old, _ := a.readSyncConfig()
	b := syncFixture(t, remote, "新书库")
	runSync(t, b)
	created, _ := b.readSyncConfig()
	if created.RemoteID == old.RemoteID || created.Folder != "论文库" {
		t.Fatal("reused name connected to the renamed library", publicSyncConfig(created))
	}
	if _, err := b.Store.Document("paper"); err == nil {
		t.Fatal("new library received unrelated documents")
	}
}
func TestWebDAVConfigDefaultsIntervalsAndCredentialsStayLocal(t *testing.T) {
	remote, directory := testDAV(t)
	s := NewServer(archiveFixture(t, t.TempDir()), "test-token", "")
	s.webdav.factory = func(c webdavConfig) *davClient { return testDAVClient(c, remote) }
	config, _ := s.readSyncConfig()
	if config.Enabled || config.Folder != "论文库" || config.IntervalMinutes != 15 {
		t.Fatalf("defaults: %+v", config)
	}
	body := []byte(`{"account":"reader@example.com","password":"application-password","folder":"论文库","deviceName":"Mac","enabled":true,"intervalMinutes":7}`)
	request := httptest.NewRequest("PUT", "http://127.0.0.1/api/sync/webdav/config", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var saved webdavConfig
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Password != "" || !saved.PasswordSet || saved.IntervalMinutes != 7 || strings.Contains(response.Body.String(), "application-password") {
		t.Fatalf("public settings: %s", response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(directory, "NeoRead", "论文库")); err != nil {
		t.Fatal("save did not create folder", err)
	}
	other := NewServer(archiveFixture(t, t.TempDir()), "other", "")
	defaults, _ := other.readSyncConfig()
	if defaults.Enabled || defaults.Account != "" {
		t.Fatal("other library was enabled")
	}
	private, _ := s.readSyncConfig()
	if private.Password != "application-password" {
		t.Fatal("application password not saved locally")
	}
}
