package neoread

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDocumentMetadataPatch(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	for _, body := range []string{`{"title":" "}`, `{"title":null}`, `{"author":null}`, `{"title":"bad\ntext"}`, `{"author":9}`} {
		if w := request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(body)); w.Code != 400 {
			t.Fatalf("accepted %s: %d", body, w.Code)
		}
	}
	w := request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"title":" New title ","author":" New author "}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got, _ := s.Store.Document(d.ID)
	if got.Title != "New title" || got.Author != "New author" || got.LastOpenedAt != d.LastOpenedAt || got.CategorySource != d.CategorySource {
		t.Fatalf("metadata patch: %+v", got)
	}
}
func TestDeleteDocumentCancelsWorkAndCleansOwnedData(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	_, err := s.Store.DB.Exec(`INSERT INTO search_index(document_id,href,content) VALUES(?, 'p1', 'hello');`, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Store.DB.Exec(`INSERT INTO annotations VALUES('note', ?, '{}')`, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Store.DB.Exec(`INSERT INTO messages VALUES('message', ?, '{}', 'now')`, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Store.DB.Exec(`INSERT INTO translations VALUES(?, 'block', 'hash', 'complete', '{}', 0)`, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.Store.Root, "cache", d.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, finish, err := s.beginDocumentTask(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		<-ctx.Done()
		// A final write before task exit must also be removed by deletion.
		_ = os.WriteFile(filepath.Join(dir, "last-write"), []byte("pending"), 0600)
		finish()
	}()
	w := request(t, s, "DELETE", "/api/documents/"+d.ID, nil)
	if w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("did not join document task")
	}
	for _, table := range []string{"documents", "annotations", "messages", "translations", "document_processing", "search_index"} {
		var count int
		if err := s.Store.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s: count=%d err=%v", table, count, err)
		}
	}
	for _, path := range []string{dir, s.Store.File(d)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("left files: %s %v", path, err)
		}
	}
	if _, _, err := s.beginDocumentTask(context.Background(), d.ID); err == nil {
		t.Fatal("deleted document can start work")
	}
	if w := request(t, s, "DELETE", "/api/documents/"+d.ID, nil); w.Code != 204 {
		t.Fatal("retry must be idempotent")
	}
}
func TestDeleteDocumentRollsBackOnDatabaseFailure(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	_, err := s.Store.DB.Exec(`INSERT INTO search_index(document_id,href,content) VALUES(?, 'p1', 'keep');`, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Store.DB.Exec(`CREATE TRIGGER fail_delete BEFORE DELETE ON documents BEGIN SELECT RAISE(ABORT, 'blocked'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if w := request(t, s, "DELETE", "/api/documents/"+d.ID, nil); w.Code != 500 {
		t.Fatal(w.Code)
	}
	if _, err := s.Store.Document(d.ID); err != nil {
		t.Fatal("lost document", err)
	}
	if _, err := os.Stat(s.Store.File(d)); err != nil {
		t.Fatal("lost original", err)
	}
	var count int
	s.Store.DB.QueryRow("SELECT count(*) FROM search_index WHERE document_id=?", d.ID).Scan(&count)
	if count != 1 {
		t.Fatal("lost search index")
	}
}

func TestDeleteSerializesReimportAndRejectsNewBackgroundWork(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	ctx, finish, err := s.beginDocumentTask(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- request(t, s, "DELETE", "/api/documents/"+d.ID, nil).Code }()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		finish()
		t.Fatal("deletion did not cancel work")
	}
	if s.importMu.TryLock() {
		s.importMu.Unlock()
		t.Error("reimport can race with file cleanup")
	}
	if _, end, err := s.beginDocumentTask(context.Background(), d.ID); err == nil {
		end()
		t.Error("task started while deleting")
	}
	finish()
	if code := <-done; code != 204 {
		t.Fatal(code)
	}
	restored := organizationDoc(t, s)
	if restored.ID != d.ID {
		t.Fatal("document ID should be stable on reimport")
	}
	if _, err := os.Stat(s.Store.File(restored)); err != nil {
		t.Fatal("reimported file lost", err)
	}
}

func TestDocumentNavigationPersistenceAndConflicts(t *testing.T) {
	s := testServer(t)
	imported := upload(t, s, "reading-notes.pdf", sample(t, "reading-notes.pdf"))
	var d Document
	if err := json.Unmarshal(imported.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	path := "/api/documents/" + d.ID + "/navigation"
	get := request(t, s, "GET", path, nil)
	if get.Code != 200 || !strings.Contains(get.Body.String(), `"revision":0`) {
		t.Fatal(get.Body.String())
	}
	body := `{"revision":0,"entries":{"outline:0":{"label":"新标题","kind":"bookmark","parentId":null,"collapsed":true}}}`
	first := request(t, s, "PUT", path, strings.NewReader(body))
	if first.Code != 200 {
		t.Fatal(first.Code, first.Body.String())
	}
	conflict := request(t, s, "PUT", path, strings.NewReader(body))
	if conflict.Code != 409 {
		t.Fatal("stale write accepted", conflict.Code)
	}
	for revision := 1; revision <= 2; revision++ {
		next := request(t, s, "PUT", path, strings.NewReader(fmt.Sprintf(`{"revision":%d,"entries":{"outline:0":{"label":"保留","deleted":true}}}`, revision)))
		if next.Code != 200 {
			t.Fatal(next.Code, next.Body.String())
		}
	}
	get = request(t, s, "GET", path, nil)
	if !strings.Contains(get.Body.String(), `"revision":3`) || !strings.Contains(get.Body.String(), "保留") {
		t.Fatal(get.Body.String())
	}
	if !strings.Contains(get.Body.String(), `"deleted":true`) {
		t.Fatal("deleted state lost", get.Body.String())
	}
	// A fresh database connection sees the same saved edits.
	reopened, err := OpenStore(s.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	var revision int
	if err = reopened.DB.QueryRow("SELECT revision FROM document_navigation WHERE document_id=?", d.ID).Scan(&revision); err != nil || revision != 3 {
		t.Fatal(revision, err)
	}
	for _, bad := range []string{
		`{"revision":3,"entries":{"a":{"parentId":"b"},"b":{"parentId":"a"}}}`,
		`{"revision":3,"entries":{"a":{"label":" "}}}`,
		`{"revision":3,"entries":{"a":{"kind":"script"}}}`,
		`{"revision":3,"entries":{"__proto__":{}}}`,
	} {
		if w := request(t, s, "PUT", path, strings.NewReader(bad)); w.Code != 400 {
			t.Fatal("accepted invalid navigation", bad, w.Code)
		}
	}
}

func TestBodyReadPositionKeepsPeakAndCurrentLocationAfterReopening(t *testing.T) {
	s := testServer(t)
	bodyProgressPaper(t, s.Store)
	for _, step := range []struct {
		body string
		peak float64
	}{
		{`{"progress":{"type":"pdf","page":1},"percentage":0.1}`, 0},
		{`{"progress":{"type":"pdf","page":5,"y":0.4},"percentage":0.5}`, 4.4},
		{`{"progress":{"type":"pdf","page":2,"y":0.1},"percentage":0.2}`, 4.4},
		{`{"progress":{"type":"pdf","page":1},"percentage":0.1,"bodyReadPosition":6.7}`, 6.7},
	} {
		w := request(t, s, "PATCH", "/api/documents/paper", strings.NewReader(step.body))
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		doc, err := s.Store.Document("paper")
		if err != nil || doc.BodyReadPosition != step.peak {
			t.Fatalf("peak lost: %v %v", doc.BodyReadPosition, err)
		}
	}
	s.Store.DB.Close()
	restored, err := OpenStore(s.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.DB.Close()
	doc, err := restored.Document("paper")
	if err != nil || doc.BodyReadPosition != 6.7 || !strings.Contains(string(doc.Progress), `"page":1`) {
		t.Fatalf("restored peak or current location changed: %+v %v", doc, err)
	}
}

func TestMarkingUnreadClearsPeakLocationAndPercentage(t *testing.T) {
	s := testServer(t)
	bodyProgressPaper(t, s.Store)
	if response := request(t, s, "PATCH", "/api/documents/paper", strings.NewReader(`{"progress":{"type":"pdf","page":6,"y":0.4},"percentage":0.6}`)); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if response := request(t, s, "PATCH", "/api/documents/paper", strings.NewReader(`{"readingStatus":"unread"}`)); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	doc, err := s.Store.Document("paper")
	if err != nil || doc.ReadingStatus != "unread" || doc.Percentage != 0 || doc.BodyReadPosition != 0 || len(doc.Progress) != 0 {
		t.Fatal(doc, err)
	}
	var reset string
	if err := s.Store.DB.QueryRow("SELECT reading_reset_at FROM documents WHERE id='paper'").Scan(&reset); err != nil || reset == "" {
		t.Fatal("reset identity was not saved", err)
	}
}

func TestBodyReadPositionMigratesExistingReadingLocation(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	bodyProgressPaper(t, store)
	if _, err := store.DB.Exec(`UPDATE documents SET percentage=.5,progress='{"type":"pdf","page":4,"y":0.3}'; ALTER TABLE documents DROP COLUMN body_read_position`); err != nil {
		t.Fatal(err)
	}
	store.DB.Close()
	reopened, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	doc, err := reopened.Document("paper")
	if err != nil || doc.BodyReadPosition != 3.3 {
		t.Fatalf("migration: %+v %v", doc, err)
	}
}

func bodyProgressPaper(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.DB.Exec(`INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at,library) VALUES('paper','pdf','Paper','Author',8,'2021-02-03','2021-02-03','papers')`); err != nil {
		t.Fatal(err)
	}
}

func TestBodyEndNeedsCompleteLayoutAndIgnoresRunningHeaders(t *testing.T) {
	layout := layoutManifest{Pages: 10, ParsedPages: []int{1, 7}, Blocks: []PDFBlock{
		{Page: 1, Label: "header", Text: "References"},
		{Page: 7, Label: "paragraph_title", Text: "7 References"},
	}}
	layout.Blocks[1].Bounds.Y = .3
	if end := layoutBodyEnd(layout); end != nil {
		t.Fatalf("partial layout reported completion: %+v", end)
	}
	for page := 2; page <= 10; page++ {
		layout.ParsedPages = append(layout.ParsedPages, page)
	}
	if end := layoutBodyEnd(layout); end == nil || end.Page != 7 || end.Y != .3 {
		t.Fatalf("reference start: %+v", end)
	}
	layout.Blocks = layout.Blocks[:1]
	if end := layoutBodyEnd(layout); end == nil || end.Page != 10 || end.Y != 1 {
		t.Fatalf("header-only fallback: %+v", end)
	}
}

func TestParsedBodyEndNotifiesOpenLibrary(t *testing.T) {
	s, p := processingFixture(t)
	defer s.Close()
	request(t, s, "PUT", "/api/preferences/library", strings.NewReader(`{"papers":{"autoLookup":false}}`))
	before := s.connectorRevision.Load()
	if err := s.learnPDF(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	doc, err := s.Store.Document("doc")
	if err != nil || doc.BodyEnd == nil || s.connectorRevision.Load() <= before {
		t.Fatalf("unpublished body end: %+v %v", doc, err)
	}
}

func TestMoveParsedBookInitializesBodyEnd(t *testing.T) {
	s := testServer(t)
	bodyProgressPaper(t, s.Store)
	if _, err := s.Store.DB.Exec(`UPDATE documents SET library='books' WHERE id='paper'`); err != nil {
		t.Fatal(err)
	}
	doc, _ := s.Store.Document("paper")
	if err := os.WriteFile(s.Store.File(doc), testPDF(2), 0600); err != nil {
		t.Fatal(err)
	}
	dir := s.analysisDir("paper")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	block := PDFBlock{ID: "p2-b1", Page: 2, Label: "paragraph_title", Text: "References"}
	block.Bounds.Y = .6
	block.Bounds.Width = .8
	block.Bounds.Height = .1
	manifest := layoutManifest{Pages: 2, Blocks: []PDFBlock{block}}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(jsonText(manifest)), 0600); err != nil {
		t.Fatal(err)
	}
	out := request(t, s, "PATCH", "/api/documents/paper", strings.NewReader(`{"library":"papers"}`))
	var moved Document
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &moved) != nil || moved.BodyEnd == nil || moved.BodyEnd.Page != 2 || moved.BodyEnd.Y != .6 {
		t.Fatalf("missing restored body end: %d %s", out.Code, out.Body.String())
	}
}

func listTrash(t *testing.T, s *Server, library string) []Document {
	t.Helper()
	w := request(t, s, "GET", "/api/trash?library="+library, nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var docs []Document
	if err := json.Unmarshal(w.Body.Bytes(), &docs); err != nil {
		t.Fatal(err)
	}
	return docs
}
func TestTrashKeepsDataUntilPurged(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	if _, err := s.Store.DB.Exec(`INSERT INTO annotations VALUES('note', ?, '{}')`, d.ID); err != nil {
		t.Fatal(err)
	}
	if w := request(t, s, "POST", "/api/documents/"+d.ID+"/trash", nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	docs, _ := s.Store.Documents()
	if len(docs) != 0 {
		t.Fatalf("trashed document still listed: %+v", docs)
	}
	trash := listTrash(t, s, "books")
	if len(trash) != 1 || trash[0].ID != d.ID || trash[0].DeletedAt == "" {
		t.Fatalf("trash: %+v", trash)
	}
	if len(listTrash(t, s, "papers")) != 0 {
		t.Fatal("trash is not separated by library")
	}
	if _, _, err := s.beginDocumentTask(context.Background(), d.ID); err == nil {
		t.Fatal("trashed document can start work")
	}
	var notes int
	s.Store.DB.QueryRow("SELECT count(*) FROM annotations WHERE document_id=?", d.ID).Scan(&notes)
	if _, err := os.Stat(s.Store.File(d)); err != nil || notes != 1 {
		t.Fatalf("trash removed data: %v notes=%d", err, notes)
	}
	// Repeating the request is harmless.
	if w := request(t, s, "POST", "/api/documents/"+d.ID+"/trash", nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := request(t, s, "POST", "/api/documents/"+d.ID+"/restore", nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if docs, _ = s.Store.Documents(); len(docs) != 1 || docs[0].DeletedAt != "" {
		t.Fatalf("restore: %+v", docs)
	}
	if w := request(t, s, "POST", "/api/documents/missing/trash", nil); w.Code != 404 {
		t.Fatalf("missing: %d", w.Code)
	}
}
func TestTrashStopsAndRequeuesRunningWork(t *testing.T) {
	s := testServer(t)
	d := paperDoc(t, s, sample(t, "reading-notes.pdf"))
	p, _ := s.Store.processing(d.ID)
	p.Status = "running"
	if err := s.Store.saveProcessing(p); err != nil {
		t.Fatal(err)
	}
	ctx, finish, err := s.beginDocumentTask(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-ctx.Done()
		// The worker reports an interrupted run as failed before it exits.
		p.Status, p.Detail = "failed", "PDF 解析未完成"
		_ = s.Store.saveProcessing(p)
		finish()
	}()
	if w := request(t, s, "POST", "/api/documents/"+d.ID+"/trash", nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got, _ := s.Store.processing(d.ID)
	if got.Status != "queued" {
		t.Fatalf("interrupted work not requeued: %+v", got)
	}
	// The processing queue skips trashed documents.
	var queued int
	s.Store.DB.QueryRow("SELECT count(*) FROM document_processing p JOIN documents d ON d.id=p.document_id WHERE p.status='queued' AND d.deleted_at=''").Scan(&queued)
	if queued != 0 {
		t.Fatal("trashed document is still eligible for processing")
	}
}
func TestReimportRestoresFromTrash(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	request(t, s, "POST", "/api/documents/"+d.ID+"/trash", nil)
	w := upload(t, s, "reading-notes.pdf", sample(t, "reading-notes.pdf"))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got, _ := s.Store.Document(d.ID)
	if got.DeletedAt != "" {
		t.Fatal("reimport left the document in the trash")
	}
}
func TestEmptyTrashPurgesOnlyThatLibrary(t *testing.T) {
	s := testServer(t)
	book := organizationDoc(t, s)
	w := uploadTo(t, s, "papers", "paper.pdf", testPDF(2))
	var paper Document
	_ = json.Unmarshal(w.Body.Bytes(), &paper)
	for _, id := range []string{book.ID, paper.ID} {
		if w := request(t, s, "POST", "/api/documents/"+id+"/trash", nil); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	if w := request(t, s, "DELETE", "/api/trash?library=other", nil); w.Code != 400 {
		t.Fatalf("unknown library: %d", w.Code)
	}
	w = request(t, s, "DELETE", "/api/trash?library=papers", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"removed":1`) {
		t.Fatal(w.Body.String())
	}
	if _, err := s.Store.Document(paper.ID); err == nil {
		t.Fatal("paper not purged")
	}
	if _, err := os.Stat(s.Store.File(paper)); !os.IsNotExist(err) {
		t.Fatal("paper file kept")
	}
	if got, err := s.Store.Document(book.ID); err != nil || got.DeletedAt == "" {
		t.Fatalf("other library touched: %+v %v", got, err)
	}
	if w = request(t, s, "DELETE", "/api/documents/"+book.ID, nil); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if len(listTrash(t, s, "books")) != 0 {
		t.Fatal("purged document still in trash")
	}
}
func TestRestoreRequeuesWorkLeftRunning(t *testing.T) {
	s := testServer(t)
	d := paperDoc(t, s, sample(t, "reading-notes.pdf"))
	request(t, s, "POST", "/api/documents/"+d.ID+"/trash", nil)
	p, _ := s.Store.processing(d.ID)
	p.Status = "running"
	_ = s.Store.saveProcessing(p)
	if w := request(t, s, "POST", "/api/documents/"+d.ID+"/restore", nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for {
		got, _ := s.Store.processing(d.ID)
		if got.Status == "queued" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restore left %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
