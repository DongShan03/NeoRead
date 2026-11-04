package neoread

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"golang.org/x/net/webdav"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestWebDAVCloudLibraryDiscoveryIsReadOnlyAndKeepsRemoteIdentity(t *testing.T) {
	remote, directory := testDAV(t)
	s := syncFixture(t, remote, "Mac")
	s.Token = "test-secret"
	liveID, deletedID := strings.Repeat("a", 32), strings.Repeat("b", 32)
	root := filepath.Join(directory, "NeoRead", ".libraries")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, location := range []syncLocation{{ID: liveID, Folder: "云端研究库"}, {ID: deletedID, Folder: "已删除", Deleted: true}} {
		if err := os.WriteFile(filepath.Join(root, location.ID+".json"), []byte(jsonText(location)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s.webdav.factory = func(c webdavConfig) *davClient {
		client := testDAVClient(c, remote)
		client.http.Transport = davTestTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" && r.Method != "PROPFIND" {
				t.Errorf("discovery mutated the cloud: %s", r.Method)
			}
			return http.DefaultTransport.RoundTrip(r)
		})
		return client
	}
	response := request(t, s, "GET", "/api/sync/webdav/libraries", nil)
	var libraries []webdavLibrary
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &libraries) != nil || len(libraries) != 1 || libraries[0].ID != liveID {
		t.Fatal(response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "application-password") {
		t.Fatal("credentials leaked")
	}
	response = request(t, s, "GET", "/api/sync/webdav/libraries/"+liveID, nil)
	var config webdavConfig
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &config) != nil || config.RemoteID != liveID || config.Folder != "云端研究库" || !config.Enabled || config.Password != "" {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request(t, s, "GET", "/api/sync/webdav/libraries/"+deletedID, nil)
	if response.Code == 200 {
		t.Fatal("deleted library can be imported")
	}
	response = request(t, s, "GET", "/api/sync/webdav/libraries/invalid", nil)
	if response.Code != 400 {
		t.Fatal("invalid identity accepted", response.Code)
	}
	local, err := s.readSyncConfig()
	if err != nil || local.RemoteID != "" || local.Folder != "论文库" {
		t.Fatal("discovery changed local sync target", local, err)
	}
}

func TestWebDAVEmptyCloudCatalogDoesNotCreateAnything(t *testing.T) {
	remote, directory := testDAV(t)
	s := syncFixture(t, remote, "Mac")
	libraries, err := s.remoteLibraries(t.Context())
	if err != nil || len(libraries) != 0 {
		t.Fatal(libraries, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "NeoRead")); !os.IsNotExist(err) {
		t.Fatal("discovery created a directory", err)
	}
}

func TestWebDAVImportedCloudLibraryDownloadsOnStartupWithSyncEnabled(t *testing.T) {
	remote, _ := testDAV(t)
	a := syncFixture(t, remote, "Mac")
	syncDocumentFixture(t, a.Store, "cloud-paper", "研究")
	runSync(t, a)
	source, _ := a.readSyncConfig()
	config, err := a.remoteLibraryConfig(t.Context(), source.RemoteID)
	if err != nil || !config.Enabled {
		t.Fatal(config, err)
	}
	b := testServer(t)
	b.webdav.factory = func(c webdavConfig) *davClient { return testDAVClient(c, remote) }
	b.webdav.beforeApply = func(context.Context) error { return nil }
	if err := b.writeSyncConfig(config); err != nil {
		t.Fatal(err)
	}
	b.StartWebDAV(t.Context())
	waitSyncPhase(t, b, "complete")
	if _, err := b.Store.Document("cloud-paper"); err != nil {
		t.Fatal("cloud-only document not downloaded", err)
	}
	connected, err := b.readSyncConfig()
	if err != nil || !connected.Enabled || connected.RemoteID != source.RemoteID {
		t.Fatal("import did not retain the enabled cloud identity", connected, err)
	}
	if _, err := b.Store.DB.Exec("UPDATE document_notes SET body='本地继续修改' WHERE document_id='cloud-paper'"); err != nil {
		t.Fatal(err)
	}
	runSync(t, b)
	runSync(t, a)
	var note string
	if err := a.Store.DB.QueryRow("SELECT body FROM document_notes WHERE document_id='cloud-paper'").Scan(&note); err != nil || note != "本地继续修改" {
		t.Fatal("imported library did not continue pushing changes", note, err)
	}
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
func TestWebDAVFailedRendererSaveDoesNotApplyAndCanRetry(t *testing.T) {
	remote, _ := testDAV(t)
	a, b := syncFixture(t, remote, "Mac"), syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	runSync(t, a)
	b.webdav.beforeApply = nil
	if err := b.beginWebDAVSync(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := waitSyncPhase(t, b, "ready")
	body, _ := json.Marshal(map[string]any{"token": status.ApplyToken, "saved": false})
	req := httptest.NewRequest("POST", "http://127.0.0.1/api/sync/webdav/apply", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+b.Token)
	out := httptest.NewRecorder()
	b.Handler().ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	b.webdav.mu.Lock()
	done := b.webdav.done
	b.webdav.mu.Unlock()
	<-done
	status = b.syncStatus()
	if status.Phase != "failed" || !status.Failed || status.ApplyToken != "" {
		t.Fatal("save failure left the run stuck or unreported", status)
	}
	if _, err := b.Store.Document("paper"); err == nil {
		t.Fatal("incoming content was applied after a failed local save")
	}
	b.webdav.beforeApply = func(context.Context) error { return nil }
	runSync(t, b)
	if _, err := b.Store.Document("paper"); err != nil || b.syncStatus().Failed {
		t.Fatal("save failure could not recover", err, b.syncStatus())
	}
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

func TestWebDAVReadingResetDoesNotRestoreAnOfflineDevicesOldPeak(t *testing.T) {
	a := syncRow{"id": "a", "created_at": "2026-11-03T10:00:00Z", "last_opened_at": "2026-11-03T10:00:00Z", "progress": "", "percentage": 0.0, "body_read_position": 0.0, "reading_reset_at": "2026-11-04T10:00:00Z", "reading_status": "unread"}
	b := syncRow{"id": "a", "created_at": "2026-11-03T10:00:00Z", "last_opened_at": "2026-11-04T11:00:00Z", "progress": "old location", "percentage": 0.8, "body_read_position": 9.0, "reading_reset_at": "", "reading_status": "reading"}
	merged, ok := mergeSyncDocumentRow(nil, []syncRow{a}, []syncRow{b})
	if !ok || merged["progress"] != "" || syncNumber(merged["body_read_position"]) != 0 || merged["reading_status"] != "unread" || merged["reading_reset_at"] != a["reading_reset_at"] {
		t.Fatal("old progress overrode an explicit reset", merged, ok)
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

func TestWebDAVTransferTotalsCountBothDirectionsAndResetBeforeNextRun(t *testing.T) {
	remote, _ := testDAV(t)
	a, b := syncFixture(t, remote, "Mac"), syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	runSync(t, a)
	syncDocumentFixture(t, b.Store, "other", "分类")
	var uploaded, downloaded int64
	var firstRequest, reset, missingTotal bool
	b.webdav.factory = func(c webdavConfig) *davClient {
		client := testDAVClient(c, remote)
		client.http.Transport = davTestTransport(func(r *http.Request) (*http.Response, error) {
			status := b.syncStatus()
			if !firstRequest {
				reset = status.Transferred == 0 && status.TotalBytes == nil
				firstRequest = true
			}
			response, err := http.DefaultTransport.RoundTrip(r)
			if err == nil && strings.Contains(r.URL.Path, "/objects/") {
				if r.Method == "PUT" {
					uploaded += r.ContentLength
				} else if r.Method == "GET" {
					downloaded += response.ContentLength
				}
				if (r.Method == "PUT" || r.Method == "GET") && status.TotalBytes == nil {
					missingTotal = true
				}
			}
			return response, err
		})
		return client
	}
	run := func() webdavStatus {
		if err := b.beginWebDAVSync(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		b.webdav.mu.Lock()
		done := b.webdav.done
		b.webdav.mu.Unlock()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("sync did not finish")
		}
		b.webdav.operation.Lock()
		b.webdav.operation.Unlock()
		status := b.syncStatus()
		if status.Phase != "complete" {
			t.Fatal(status)
		}
		return status
	}
	status := run()
	if !reset || missingTotal || uploaded <= 0 || downloaded <= 0 || int64(status.Transferred) != uploaded+downloaded || status.TotalBytes == nil || *status.TotalBytes != uploaded+downloaded {
		t.Fatalf("progress = %+v; uploads %d downloads %d reset %v", status, uploaded, downloaded, reset)
	}
	uploaded, downloaded, firstRequest, reset = 0, 0, false, false
	status = run()
	if !reset || uploaded != 0 || downloaded != 0 || status.Transferred != 0 || status.TotalBytes == nil || *status.TotalBytes != 0 {
		t.Fatal("reused chunks or previous run were counted", status, uploaded, downloaded, reset)
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

func TestWebDAVStartupSyncPushesAndPullsBeforeTheNextInterval(t *testing.T) {
	remote, _ := testDAV(t)
	a, b := syncFixture(t, remote, "Mac"), syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	runSync(t, a)
	runSync(t, b)
	if _, err := a.Store.DB.Exec("UPDATE document_notes SET body='启动时推送的本地笔记' WHERE document_id='paper'"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.DB.Exec("UPDATE documents SET title='启动时拉取的云端标题' WHERE id='paper'"); err != nil {
		t.Fatal(err)
	}
	runSync(t, b)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a.StartWebDAV(ctx)
	status := waitSyncPhase(t, a, "complete")
	if status.Manual || status.RunID == "" || status.Dirty {
		t.Fatal("startup did not run an automatic sync", status)
	}
	runSync(t, b)
	for _, server := range []*Server{a, b} {
		doc, err := server.Store.Document("paper")
		if err != nil || doc.Title != "启动时拉取的云端标题" {
			t.Fatal(doc, err)
		}
		var note string
		if err := server.Store.DB.QueryRow("SELECT body FROM document_notes WHERE document_id='paper'").Scan(&note); err != nil || note != "启动时推送的本地笔记" {
			t.Fatal(note, err)
		}
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
	if _, err := b.Store.DB.Exec(`UPDATE annotations SET body=json_set(body,'$.note','independent annotation') WHERE id='annotation-paper'`); err != nil {
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
	for _, server := range []*Server{a, b} {
		if err = server.Store.DB.QueryRow(`SELECT json_extract(body,'$.note') FROM annotations WHERE id='annotation-paper'`).Scan(&note); err != nil || note != "independent annotation" {
			t.Fatal("selecting a note version lost an independent annotation", note, err)
		}
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

func TestWebDAVLibraryOrganizationDoesNotOverrideApplicationDisplayPreferences(t *testing.T) {
	remote, _ := testDAV(t)
	a, b := syncFixture(t, remote, "Mac"), syncFixture(t, remote, "Windows")
	for _, server := range []*Server{a, b} {
		if err := server.SetApplicationSettingsPath(filepath.Join(t.TempDir(), "application-settings.json"), ""); err != nil {
			t.Fatal(err)
		}
	}
	// Establish a common baseline before editing app-wide display settings.
	runSync(t, a)
	runSync(t, b)
	if err := a.saveApplicationPreference("preferences:library", map[string]any{"papers": map[string]any{"showAuthors": true, "categories": []any{"共享分类"}}}); err != nil {
		t.Fatal(err)
	}
	before, err := b.readApplicationPreference("preferences:library")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := before["papers"].(map[string]any); !ok {
		before["papers"] = map[string]any{}
	}
	before["papers"].(map[string]any)["showAuthors"] = false
	if err := b.saveApplicationPreference("preferences:library", before); err != nil {
		t.Fatal(err)
	}
	runSync(t, a)
	runSync(t, b)
	prefs, err := readPreference(b.Store.DB, "preferences:library")
	if err != nil {
		t.Fatal(err)
	}
	papers := prefs["papers"].(map[string]any)
	if papers["showAuthors"] != false || papers["categories"].([]any)[0] != "共享分类" {
		t.Fatal("sync overwrote machine display settings or lost organization", papers)
	}
	if err := a.saveApplicationPreference("preferences:library", map[string]any{"papers": map[string]any{"showAuthors": true}}); err != nil {
		t.Fatal(err)
	}
	runSync(t, a)
	runSync(t, b)
	prefs, err = readPreference(b.Store.DB, "preferences:library")
	if err != nil {
		t.Fatal(err)
	}
	papers = prefs["papers"].(map[string]any)
	if _, exists := papers["categories"]; exists || papers["showAuthors"] != false {
		t.Fatal("remote organization deletion lost app-wide display settings", papers)
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

func TestWebDAVDeletedLibraryCannotBeRecreatedByAnOfflineDevice(t *testing.T) {
	remote, directory := testDAV(t)
	a, b := syncFixture(t, remote, "Mac"), syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	runSync(t, a)
	runSync(t, b)
	before, _ := a.readSyncConfig()
	if err := a.deleteRemoteLibrary(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "NeoRead", "论文库")); !os.IsNotExist(err) {
		t.Fatal("cloud library still exists", err)
	}
	config, _ := b.readSyncConfig()
	if err := b.runWebDAV(t.Context(), config); err == nil {
		t.Fatal("offline device recreated deleted library")
	}
	if _, err := b.Store.Document("paper"); err != nil {
		t.Fatal("failed sync deleted local content", err)
	}
	c := syncFixture(t, remote, "New library")
	runSync(t, c)
	created, _ := c.readSyncConfig()
	if created.RemoteID == before.RemoteID {
		t.Fatal("reused deleted library identity")
	}
	if _, err := c.Store.Document("paper"); err == nil {
		t.Fatal("new library received deleted data")
	}
}
func TestWebDAVConfigDefaultsIntervalsAndCredentialsStayLocal(t *testing.T) {
	remote, directory := testDAV(t)
	s := NewServer(archiveFixture(t, t.TempDir()), "test-token", "")
	t.Cleanup(s.Close)
	connections := 0
	s.webdav.factory = func(c webdavConfig) *davClient {
		connections++
		return testDAVClient(c, remote)
	}
	config, _ := s.readSyncConfig()
	if config.Enabled || config.Folder != "论文库" || config.IntervalMinutes != 15 {
		t.Fatalf("defaults: %+v", config)
	}
	body := []byte(`{"account":"reader@example.com","password":"application-password","folder":"论文库","deviceName":"Mac","enabled":true,"intervalMinutes":7}`)
	call := func(method, path string, body []byte) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "http://127.0.0.1/api/sync/webdav/"+path, bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer test-token")
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, request)
		return response
	}
	if response := call("PUT", "config", body); response.Code != 409 || connections != 0 {
		t.Fatal("untested configuration was saved or contacted WebDAV", response.Code)
	}
	if response := call("POST", "test", body); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	untouched, _ := s.readSyncConfig()
	if untouched.LibraryConfigured || untouched.Enabled || untouched.Account != "" {
		t.Fatal("testing changed persisted settings", publicSyncConfig(untouched))
	}
	if _, err := os.Stat(filepath.Join(directory, "NeoRead", "论文库", "library.json")); err != nil {
		t.Fatal("test did not initialize the library", err)
	}
	before := connections
	remote.Close()
	response := call("PUT", "config", body)
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
		t.Fatal("test did not create folder", err)
	}
	if connections != before {
		t.Fatal("save contacted WebDAV again")
	}
	other := NewServer(archiveFixture(t, t.TempDir()), "other", "")
	t.Cleanup(other.Close)
	defaults, _ := other.readSyncConfig()
	if defaults.Enabled || defaults.Account != "" {
		t.Fatal("other library was enabled")
	}
	private, _ := s.readSyncConfig()
	if private.Password != "application-password" {
		t.Fatal("application password not saved locally")
	}
	private.Password = ""
	private.IntervalMinutes = 10
	changed, _ := json.Marshal(private)
	if response := call("PUT", "config", changed); response.Code != 200 || connections != before {
		t.Fatal("changing the interval required a network connection", response.Code)
	}
	private.Password = "untested-password"
	changed, _ = json.Marshal(private)
	if response := call("PUT", "config", changed); response.Code != 409 || connections != before {
		t.Fatal("untested password was saved", response.Code)
	}
}

func syncAPI(t *testing.T, s *Server, method, path string) {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1"+path, nil)
	req.Header.Set("Authorization", "Bearer "+s.Token)
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, req)
	if out.Code < 200 || out.Code >= 300 {
		t.Fatal(out.Code, out.Body.String())
	}
}

func TestWebDAVDisablePreservesHistoryAndOfflineDeletion(t *testing.T) {
	remote, _ := testDAV(t)
	a, b := syncFixture(t, remote, "Mac"), syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	runSync(t, a)
	runSync(t, b)
	config, _ := a.readSyncConfig()
	before, err := a.loadSyncState(config)
	if err != nil {
		t.Fatal(err)
	}
	syncAPI(t, a, "DELETE", "/api/sync/webdav/config")
	disabled, err := a.readSyncConfig()
	if err != nil || disabled.Enabled || disabled.RemoteID != config.RemoteID {
		t.Fatal("disabling lost the connected library", publicSyncConfig(disabled), err)
	}
	if err := a.beginWebDAVSync(t.Context(), true); err == nil {
		t.Fatal("disabled library started a sync")
	}
	syncAPI(t, a, "DELETE", "/api/documents/paper")
	disabled.Enabled = true
	body, _ := json.Marshal(disabled)
	req := httptest.NewRequest("PUT", "http://127.0.0.1/api/sync/webdav/config", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.Token)
	out := httptest.NewRecorder()
	a.Handler().ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	runSync(t, a)
	runSync(t, b)
	assertSyncDocumentDeleted(t, a, nil)
	assertSyncDocumentDeleted(t, b, nil)
	after, err := a.loadSyncState(config)
	if err != nil || after.Device != before.Device || !after.Own["document/paper"].Deleted {
		t.Fatal("reenabling forgot the original deletion history", err)
	}
}

func TestWebDAVDeletionPreservesAttachmentsUsedByOtherDocuments(t *testing.T) {
	remote, _ := testDAV(t)
	a, b := syncFixture(t, remote, "Mac"), syncFixture(t, remote, "Windows")
	attachment := []byte("shared attachment")
	hash := syncBytesHash(attachment)
	for _, owner := range []string{"paper", "other"} {
		syncDocumentFixture(t, a.Store, owner, "分类")
		if _, err := a.Store.DB.Exec(`INSERT INTO paper_attachments(id,document_id,kind,title,sha256,created_at) VALUES(?,?,'file','shared',?,'now')`, owner, owner, hash); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(a.Store.Root, "attachments"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Store.Root, "attachments", hash), attachment, 0600); err != nil {
		t.Fatal(err)
	}
	runSync(t, a)
	runSync(t, b)
	syncAPI(t, a, "DELETE", "/api/documents/paper")
	runSync(t, a)
	runSync(t, b)
	body, err := os.ReadFile(filepath.Join(b.Store.Root, "attachments", hash))
	if err != nil || !bytes.Equal(body, attachment) {
		t.Fatal("deleting one document removed another document's attachment", err)
	}
	syncDocumentFixture(t, a.Store, "new-paper", "分类")
	if _, err := a.Store.DB.Exec(`INSERT INTO paper_attachments(id,document_id,kind,title,sha256,created_at) VALUES('new','new-paper','file','shared',?,'now')`, hash); err != nil {
		t.Fatal(err)
	}
	syncAPI(t, a, "DELETE", "/api/documents/other")
	runSync(t, a)
	runSync(t, b)
	if _, err := b.Store.Document("new-paper"); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(filepath.Join(b.Store.Root, "attachments", hash))
	if err != nil || !bytes.Equal(body, attachment) {
		t.Fatal("incoming document lost the shared attachment", err)
	}
	syncAPI(t, a, "DELETE", "/api/documents/new-paper")
	runSync(t, a)
	runSync(t, b)
	if _, err := os.Stat(filepath.Join(b.Store.Root, "attachments", hash)); !os.IsNotExist(err) {
		t.Fatal("last attachment reference was not removed", err)
	}
}

func TestWebDAVReplacedLegacyTranscriptClearsItsProviderContinuation(t *testing.T) {
	remote, _ := testDAV(t)
	a, b := syncFixture(t, remote, "Mac"), syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	runSync(t, a)
	runSync(t, b)
	c := documentConversation{ID: "legacy", ConnectionKey: "key", LastMessageID: "message-paper", NativeThread: "local-thread"}
	if err := saveDocumentConversation(t.Context(), b.Store.DB, "paper", "codex", c); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.DB.Exec(`UPDATE messages SET body=json_set(body,'$.content','updated legacy answer') WHERE id='message-paper'`); err != nil {
		t.Fatal(err)
	}
	runSync(t, a)
	runSync(t, b)
	got, err := b.Store.documentConversation(t.Context(), "paper", "codex", "key")
	if err != nil || got.NativeThread != "" {
		t.Fatal("legacy provider replayed an outdated transcript", got, err)
	}
}

func TestWebDAVReplacedTranscriptClearsOnlyItsProviderContinuations(t *testing.T) {
	remote, _ := testDAV(t)
	a, b := syncFixture(t, remote, "Mac"), syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	for _, session := range []string{"first", "second"} {
		if _, err := a.Store.DB.Exec(`INSERT INTO chat_sessions VALUES(?,'paper',?,'codex','now','now')`, session, session); err != nil {
			t.Fatal(err)
		}
		m := Message{ID: session, SessionID: session, DocumentID: "paper", Role: "assistant", Content: "saved answer", CreatedAt: "now"}
		if _, err := a.Store.DB.Exec("INSERT INTO messages VALUES(?,'paper',?,'now')", m.ID, jsonText(m)); err != nil {
			t.Fatal(err)
		}
	}
	runSync(t, a)
	runSync(t, b)
	for _, session := range []string{"first", "second", "first-extra"} {
		c := documentConversation{ID: session, ConnectionKey: "key", LastMessageID: session, NativeThread: "local-thread"}
		if err := saveDocumentConversation(t.Context(), b.Store.DB, "paper", conversationProvider(session, "codex"), c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Store.DB.Exec(`UPDATE messages SET body=json_set(body,'$.content','remote revision') WHERE id='first'`); err != nil {
		t.Fatal(err)
	}
	runSync(t, a)
	runSync(t, b)
	for _, session := range []string{"first", "second", "first-extra"} {
		c, err := b.Store.documentConversation(t.Context(), "paper", conversationProvider(session, "codex"), "key")
		if err != nil || (c.NativeThread == "") != (session == "first") {
			t.Fatal("wrong continuation was retained or cleared", session, c, err)
		}
	}
	history, err := b.Store.chatHistory("paper", "first")
	if err != nil || len(history) != 1 || history[0].Content != "remote revision" {
		t.Fatal("synced transcript was lost", history, err)
	}
}

func assertSyncDocumentDeleted(t *testing.T, s *Server, names []string) {
	t.Helper()
	if _, err := s.Store.Document("paper"); err == nil {
		t.Fatal("deleted document returned")
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(s.Store.Root, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Fatal("deleted file remained", name, err)
		}
	}
	for _, table := range []string{"annotations", "document_notes", "translations", "messages", "search_index"} {
		var count int
		if err := s.Store.DB.QueryRow("SELECT count(*) FROM " + table + " WHERE document_id='paper'").Scan(&count); err != nil || count != 0 {
			t.Fatal("deleted records remained", table, count, err)
		}
	}
}

func TestWebDAVTrashRestoreAndPermanentDeletionReachTheCloudAndOtherDevices(t *testing.T) {
	remote, directory := testDAV(t)
	a, b := syncFixture(t, remote, "Mac"), syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	runSync(t, a)
	runSync(t, b)
	syncAPI(t, a, "POST", "/api/documents/paper/trash")
	runSync(t, a)
	runSync(t, b)
	d, err := b.Store.Document("paper")
	if err != nil || d.DeletedAt == "" {
		t.Fatal("trash state not synchronized", d, err)
	}
	if _, err := os.Stat(b.Store.File(d)); err != nil {
		t.Fatal("trash removed original", err)
	}
	syncAPI(t, b, "POST", "/api/documents/paper/restore")
	runSync(t, b)
	runSync(t, a)
	d, err = a.Store.Document("paper")
	if err != nil || d.DeletedAt != "" {
		t.Fatal("restore not synchronized", d, err)
	}
	config, _ := a.readSyncConfig()
	state, err := a.loadSyncState(config)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for name := range state.Base["document/paper"].Files {
		names = append(names, name)
	}
	syncAPI(t, a, "DELETE", "/api/documents/paper")
	runSync(t, a)
	runSync(t, b)
	assertSyncDocumentDeleted(t, b, names)
	var manifest syncManifest
	body, err := os.ReadFile(filepath.Join(directory, "NeoRead", "论文库", "devices", state.Device+".json"))
	if err != nil || json.Unmarshal(body, &manifest) != nil || !manifest.Entries["document/paper"].Deleted {
		t.Fatal("cloud did not receive deletion", string(body), err)
	}
	c := syncFixture(t, remote, "New device")
	runSync(t, c)
	runSync(t, b)
	runSync(t, a)
	assertSyncDocumentDeleted(t, c, names)
	assertSyncDocumentDeleted(t, b, names)
}

func TestWebDAVOfflineDeletionWaitsForAConcurrentEditChoiceAndNeverResurrects(t *testing.T) {
	remote, _ := testDAV(t)
	a, b := syncFixture(t, remote, "Mac"), syncFixture(t, remote, "Windows")
	syncDocumentFixture(t, a.Store, "paper", "分类")
	runSync(t, a)
	runSync(t, b)
	config, _ := a.readSyncConfig()
	before, _ := a.loadSyncState(config)
	names := []string{}
	for name := range before.Base["document/paper"].Files {
		names = append(names, name)
	}
	syncAPI(t, a, "DELETE", "/api/documents/paper")
	a.webdav.factory = func(c webdavConfig) *davClient {
		client := testDAVClient(c, remote)
		client.http.Transport = davTestTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 503, Header: http.Header{}, Body: http.NoBody}, nil
		})
		return client
	}
	if err := a.runWebDAV(context.Background(), config); err == nil {
		t.Fatal("expected offline sync failure")
	}
	if _, err := b.Store.DB.Exec("UPDATE document_notes SET body='offline edit' WHERE document_id='paper'"); err != nil {
		t.Fatal(err)
	}
	runSync(t, b)
	a.Close()
	resumed := NewServer(a.Store, "test-token", "")
	resumed.webdav.factory = func(c webdavConfig) *davClient { return testDAVClient(c, remote) }
	resumed.webdav.beforeApply = func(context.Context) error { return nil }
	t.Cleanup(resumed.Close)
	runSync(t, resumed)
	conflicts := resumed.syncStatus().Conflicts
	if len(conflicts) != 1 || conflicts[0].CanKeepBoth {
		t.Fatal("delete/edit conflict not retained", conflicts)
	}
	var note string
	if err := b.Store.DB.QueryRow("SELECT body FROM document_notes WHERE document_id='paper'").Scan(&note); err != nil || note != "offline edit" {
		t.Fatal("unresolved edit was lost", note, err)
	}
	state, err := resumed.loadSyncState(config)
	if err != nil {
		t.Fatal(err)
	}
	deletion := ""
	for _, version := range conflicts[0].Versions {
		if version.Deleted {
			deletion = version.ID
		}
	}
	if deletion == "" {
		t.Fatal("missing deletion choice")
	}
	state.Choices[conflicts[0].Key] = syncChoice{Conflict: conflicts[0].ID, Version: deletion}
	if err := resumed.saveSyncState(config, state); err != nil {
		t.Fatal(err)
	}
	runSync(t, resumed)
	runSync(t, b)
	runSync(t, resumed)
	runSync(t, b)
	assertSyncDocumentDeleted(t, b, names)
	if len(resumed.syncStatus().Conflicts) != 0 || len(b.syncStatus().Conflicts) != 0 {
		t.Fatal("resolved deletion was offered again")
	}
}

func TestWebDAVDeletionFileRecoveryMatchesTheDatabaseCommit(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "commit"}[committed], func(t *testing.T) {
			s := NewServer(archiveFixture(t, t.TempDir()), "test-token", "")
			run := id()
			name := "papers/paper.pdf"
			backup := filepath.Join(s.syncDirectory(), "apply", run, "backup", name)
			if err := os.MkdirAll(filepath.Dir(backup), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(backup, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(syncInstallJournal{ID: run, Files: []syncInstallFile{{Name: name, Existed: true, Remove: true}}})
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
			restored, err := os.ReadFile(filepath.Join(s.Store.Root, name))
			if committed && !os.IsNotExist(err) || !committed && (err != nil || string(restored) != "original") {
				t.Fatal("deletion recovery lost or revived the original", string(restored), err)
			}
		})
	}
}

func testDAV(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	directory := t.TempDir()
	handler := &webdav.Handler{Prefix: "/dav", FileSystem: webdav.Dir(directory), LockSystem: webdav.NewMemLS()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "reader@example.com" || password != "application-password" {
			w.WriteHeader(401)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server, directory
}
func testDAVClient(config webdavConfig, server *httptest.Server) *davClient {
	d := newDAVClient(config)
	d.base, _ = url.Parse(server.URL + "/dav/NeoRead/" + config.Folder + "/")
	d.interval = 0
	return d
}
func TestDAVCreatesApplicationAndLibraryFolders(t *testing.T) {
	server, directory := testDAV(t)
	config := webdavConfig{Account: "reader@example.com", Password: "application-password", Folder: "论文库"}
	d := testDAVClient(config, server)
	if err := d.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{"NeoRead/论文库/devices", "NeoRead/论文库/objects"} {
		if info, err := os.Stat(filepath.Join(directory, folder)); err != nil || !info.IsDir() {
			t.Fatalf("missing %s: %v", folder, err)
		}
	}
	device := strings.Repeat("a", 32) + ".json"
	if err := d.put(context.Background(), "devices/"+device, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	list, err := d.devices(context.Background())
	if err != nil || len(list) != 1 || list[0].Name != device || list[0].ETag == "" {
		t.Fatalf("listing: %+v %v", list, err)
	}
	if err = d.prepare(context.Background()); err != nil {
		t.Fatalf("repeat folder creation: %v", err)
	}
}
func TestDAVDoesNotForwardApplicationPasswordToRedirect(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	d := newDAVClient(webdavConfig{Account: "reader@example.com", Password: "application-password", Folder: "论文库"})
	d.base, _ = url.Parse(redirect.URL + "/dav/NeoRead/论文库/")
	d.interval = 0
	_, err := d.get(context.Background(), "devices/a.json", 100)
	var remote *davError
	if !errors.As(err, &remote) || remote.Status != 307 || forwarded.Load() != 0 {
		t.Fatalf("redirect: %v, forwarded %d", err, forwarded.Load())
	}
}
func TestDAVPreservesRateLimitDelayAndHidesServerBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "37")
		w.WriteHeader(429)
		_, _ = w.Write([]byte("server-private-response application-password"))
	}))
	defer server.Close()
	d := testDAVClient(webdavConfig{Folder: "论文库"}, server)
	_, err := d.get(context.Background(), "devices/a.json", 100)
	var remote *davError
	if !errors.As(err, &remote) || remote.Retry != 37*time.Second || strings.Contains(err.Error(), "private") {
		t.Fatalf("rate limit: %v", err)
	}
}
func TestDAVCancellationStopsPacing(t *testing.T) {
	d := newDAVClient(webdavConfig{Folder: "论文库"})
	*d.last = time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.request(ctx, "GET", "devices/a.json", nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestDAVLibraryAndCatalogRequestsShareRateLimitPacing(t *testing.T) {
	server, _ := testDAV(t)
	d := testDAVClient(webdavConfig{Account: "reader@example.com", Password: "application-password", Folder: "论文库"}, server)
	if err := d.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	d.interval = time.Second
	*d.last = time.Time{}
	root := d.rootClient()
	if err := root.put(t.Context(), "pacing-test", []byte("test")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := d.get(ctx, "library.json", 100); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("library request did not wait after catalog request", err)
	}
}

func cachedSyncChunk(t *testing.T, s *Server, text string) (string, *syncDocument) {
	t.Helper()
	hash := syncBytesHash([]byte(text))
	if err := atomicFile(s.syncBlob(hash), []byte(text)); err != nil {
		t.Fatal(err)
	}
	doc := emptySyncDocument()
	doc.Files["cache/paper/"+text] = syncFile{Size: int64(len(text)), Chunks: []syncChunk{{Hash: hash, Size: len(text)}}}
	return hash, doc
}

func TestWebDAVCacheReclaimsOldVersionsAfterSuccessfulSync(t *testing.T) {
	remote, _ := testDAV(t)
	s := syncFixture(t, remote, "Mac")
	syncDocumentFixture(t, s.Store, "paper", "研究")
	runSync(t, s)
	config, _ := s.readSyncConfig()
	state, err := s.loadSyncState(config)
	if err != nil {
		t.Fatal(err)
	}
	name := "cache/paper/analysis/manifest.json"
	oldHash := state.Files[name].File.Chunks[0].Hash
	if err := os.WriteFile(filepath.Join(s.Store.Root, filepath.FromSlash(name)), []byte(`{"pages":1,"blocks":[],"revision":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	runSync(t, s)
	// The next snapshot refreshes the cached own-device manifest too.
	runSync(t, s)
	if _, err := os.Stat(s.syncBlob(oldHash)); !os.IsNotExist(err) {
		t.Fatal("unreferenced old chunk retained", err)
	}
	state, err = s.loadSyncState(config)
	if err != nil {
		t.Fatal(err)
	}
	current := state.Files[name].File.Chunks[0].Hash
	if _, err := os.Stat(s.syncBlob(current)); err != nil {
		t.Fatal("current chunk removed", err)
	}
	if _, err := os.Stat(s.Store.File(Document{ID: "paper", Type: "pdf"})); err != nil {
		t.Fatal("original removed", err)
	}
}

func TestWebDAVCacheKeepsOtherIdentitiesAndConflictingRemoteVersions(t *testing.T) {
	s := testServer(t)
	config := webdavConfig{Account: "reader@example.com", RemoteID: strings.Repeat("a", 32)}
	state := newSyncState()
	ownHash, own := cachedSyncChunk(t, s, "own")
	state.Own["document/paper"] = newSyncVersion(state, own, nil, "Mac")
	baseHash, base := cachedSyncChunk(t, s, "base")
	state.Base["document/paper"] = base
	fingerprintHash, fingerprint := cachedSyncChunk(t, s, "fingerprint")
	state.Files["cache/paper/fingerprint"] = syncFingerprint{File: fingerprint.Files["cache/paper/fingerprint"]}
	if err := s.saveSyncState(config, state); err != nil {
		t.Fatal(err)
	}
	pausedHash, paused := cachedSyncChunk(t, s, "paused")
	otherConfig := webdavConfig{Account: "other@example.com", RemoteID: strings.Repeat("b", 32)}
	other := newSyncState()
	other.Own["document/paper"] = newSyncVersion(other, paused, nil, "Paused")
	if err := s.saveSyncState(otherConfig, other); err != nil {
		t.Fatal(err)
	}
	remoteHash, remote := cachedSyncChunk(t, s, "conflict")
	peer := newSyncState()
	manifest := syncManifest{Format: "neoread-webdav", Version: 1, Device: peer.Device, Entries: map[string]*syncVersion{"document/paper": newSyncVersion(peer, remote, nil, "Peer")}}
	path := filepath.Join(filepath.Dir(s.syncStatePath(config)), "remotes", peer.Device+".json")
	if err := atomicFile(path, []byte(jsonText(manifest))); err != nil {
		t.Fatal(err)
	}
	orphanHash, _ := cachedSyncChunk(t, s, "orphan")
	count, _, err := s.pruneSyncCache(t.Context(), config, state)
	if err != nil || count != 1 {
		t.Fatal("cache pruning failed", count, err)
	}
	for _, hash := range []string{ownHash, baseHash, fingerprintHash, pausedHash, remoteHash} {
		if _, err := os.Stat(s.syncBlob(hash)); err != nil {
			t.Fatal("needed chunk removed", hash, err)
		}
	}
	if _, err := os.Stat(s.syncBlob(orphanHash)); !os.IsNotExist(err) {
		t.Fatal("orphan retained", err)
	}
}

func TestWebDAVCacheSkipsCollectionWhenReferencesCannotBeEstablished(t *testing.T) {
	for _, mode := range []string{"corrupt-state", "recovery", "cancelled", "outside-symlink"} {
		t.Run(mode, func(t *testing.T) {
			s := testServer(t)
			config := webdavConfig{Account: "reader@example.com", RemoteID: strings.Repeat("a", 32)}
			state := newSyncState()
			if err := s.saveSyncState(config, state); err != nil {
				t.Fatal(err)
			}
			orphanHash, _ := cachedSyncChunk(t, s, "orphan")
			ctx := t.Context()
			wantError := mode != "recovery"
			switch mode {
			case "corrupt-state":
				path := filepath.Join(s.syncDirectory(), strings.Repeat("b", 64), "state.json")
				if err := atomicFile(path, []byte("invalid")); err != nil {
					t.Fatal(err)
				}
			case "recovery":
				if err := atomicFile(filepath.Join(s.syncDirectory(), "apply.json"), []byte("pending")); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				child, cancel := context.WithCancel(ctx)
				cancel()
				ctx = child
			case "outside-symlink":
				outside := t.TempDir()
				if err := os.WriteFile(filepath.Join(outside, orphanHash), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
				objects := filepath.Join(s.syncDirectory(), "objects")
				if err := os.Rename(objects, objects+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, objects); err != nil {
					t.Skip("symlinks unavailable", err)
				}
			}
			count, _, err := s.pruneSyncCache(ctx, config, state)
			if count != 0 || (err != nil) != wantError {
				t.Fatal("unsafe pruning result", count, err)
			}
			if _, err := os.Stat(s.syncBlob(orphanHash)); err != nil {
				t.Fatal("cache removed", err)
			}
		})
	}
}

func TestWebDAVCacheFailureDoesNotFailCompletedSync(t *testing.T) {
	remote, _ := testDAV(t)
	s := syncFixture(t, remote, "Mac")
	syncDocumentFixture(t, s.Store, "paper", "研究")
	path := filepath.Join(s.syncDirectory(), strings.Repeat("c", 64), "state.json")
	if err := atomicFile(path, []byte("invalid")); err != nil {
		t.Fatal(err)
	}
	runSync(t, s)
	if _, err := s.Store.Document("paper"); err != nil {
		t.Fatal(err)
	}
}
