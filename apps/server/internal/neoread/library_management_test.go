package neoread

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// testPDF builds a minimal valid PDF with blank pages and a correct xref table.
// lowerLargePaperBytes lets small test PDFs count as large by size, so page
// counts decide.
func lowerLargePaperBytes(t *testing.T) {
	saved := largePaperBytes
	largePaperBytes = 1
	t.Cleanup(func() { largePaperBytes = saved })
}

func TestLargePaperNeedsPagesAndSize(t *testing.T) {
	for _, tc := range []struct {
		pages int
		size  int64
		want  bool
	}{
		{largePaperPages + 1, largePaperBytes + 1, true},
		{largePaperPages + 1, largePaperBytes, false},
		{largePaperPages, largePaperBytes + 1, false},
		{0, largePaperBytes + 1, true},
		{0, largePaperBytes, false},
	} {
		if got := largePaper(tc.pages, tc.size); got != tc.want {
			t.Errorf("largePaper(%d, %d) = %t", tc.pages, tc.size, got)
		}
	}
}

func testPDF(pages int) []byte {
	var b bytes.Buffer
	offsets := []int{}
	object := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	b.WriteString("%PDF-1.4\n")
	object("<< /Type /Catalog /Pages 2 0 R >>")
	kids := []string{}
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", i+3))
	}
	object(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages))
	for range pages {
		object("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>")
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}
func uploadTo(t *testing.T, s *Server, library, name string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	if library != "" {
		_ = m.WriteField("library", library)
	}
	f, _ := m.CreateFormFile("file", name)
	f.Write(data)
	m.Close()
	r := httptest.NewRequest("POST", "http://127.0.0.1:17840/api/documents", &body)
	r.Header.Set("Authorization", "Bearer test-secret")
	r.Header.Set("Content-Type", m.FormDataContentType())
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func TestPDFPageCount(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []int{1, 7} {
		path := filepath.Join(dir, fmt.Sprintf("%d.pdf", n))
		if err := os.WriteFile(path, testPDF(n), 0600); err != nil {
			t.Fatal(err)
		}
		if got, err := pdfPageCount(path); err != nil || got != n {
			t.Fatalf("pages %d: %d %v", n, got, err)
		}
	}
	path := filepath.Join(dir, "sample.pdf")
	if err := os.WriteFile(path, sample(t, "reading-notes.pdf"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := pdfPageCount(path); err != nil || got < 1 {
		t.Fatalf("sample: %d %v", got, err)
	}
	broken := filepath.Join(dir, "broken.pdf")
	if err := os.WriteFile(broken, []byte("%PDF-1.4\nnot a pdf"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := pdfPageCount(broken); err == nil {
		t.Fatal("broken PDF counted")
	}
}
func TestPaperLibraryImportLimits(t *testing.T) {
	s := testServer(t)
	w := uploadTo(t, s, "papers", "paper.pdf", testPDF(3))
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var d Document
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Library != "papers" || d.Category != "paper" || d.CategorySource != "manual" || d.ClassificationStatus != "done" {
		t.Fatalf("paper import: %+v", d)
	}
	if w = uploadTo(t, s, "papers", "book.epub", sample(t, "the-art-of-reading.epub")); w.Code != 400 || !strings.Contains(w.Body.String(), "只支持 PDF") {
		t.Fatalf("epub accepted into papers: %d %s", w.Code, w.Body.String())
	}
	if w = uploadTo(t, s, "papers", "long-small.pdf", testPDF(largePaperPages+3)); w.Code != 201 {
		t.Fatalf("long but small PDF asked: %d %s", w.Code, w.Body.String())
	}
	lowerLargePaperBytes(t)
	if w = uploadTo(t, s, "papers", "longer.pdf", testPDF(largePaperPages+2)); w.Code != 409 || !strings.Contains(w.Body.String(), "large-paper") {
		t.Fatalf("long PDF accepted: %d %s", w.Code, w.Body.String())
	}
	if w = uploadTo(t, s, "shelf", "x.pdf", testPDF(1)); w.Code != 400 {
		t.Fatalf("unknown library accepted: %d", w.Code)
	}
	// The same file in the book library is accepted there without limits.
	if w = uploadTo(t, s, "", "long.pdf", testPDF(largePaperPages+1)); w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil || d.Library != "books" {
		t.Fatalf("default library: %+v %v", d, err)
	}
	if reason := paperLimit("pdf", largePaperBytes+1, ""); reason != "" {
		t.Fatalf("size limit: %q", reason)
	}
}
func TestMoveDocumentBetweenLibraries(t *testing.T) {
	s := testServer(t)
	long := uploadTo(t, s, "books", "long.pdf", testPDF(largePaperPages+1))
	short := uploadTo(t, s, "books", "short.pdf", testPDF(2))
	var longDoc, shortDoc Document
	_ = json.Unmarshal(long.Body.Bytes(), &longDoc)
	_ = json.Unmarshal(short.Body.Bytes(), &shortDoc)
	if w := request(t, s, "PATCH", "/api/documents/"+longDoc.ID, strings.NewReader(`{"library":"papers"}`)); w.Code != 200 {
		t.Fatalf("long PDF moved: %d", w.Code)
	}
	if w := request(t, s, "PATCH", "/api/documents/"+shortDoc.ID, strings.NewReader(`{"library":"nowhere"}`)); w.Code != 400 {
		t.Fatalf("unknown library: %d", w.Code)
	}
	if w := request(t, s, "PATCH", "/api/documents/"+shortDoc.ID, strings.NewReader(`{"library":"papers"}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got, _ := s.Store.Document(shortDoc.ID)
	if got.Library != "papers" || got.Category != "paper" || got.CategorySource != "manual" || got.LastOpenedAt != shortDoc.LastOpenedAt {
		t.Fatalf("moved: %+v", got)
	}
	if w := request(t, s, "PATCH", "/api/documents/"+shortDoc.ID, strings.NewReader(`{"library":"books"}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if got, _ = s.Store.Document(shortDoc.ID); got.Library != "books" || got.Category != "paper" {
		t.Fatalf("moved back: %+v", got)
	}
}
func TestLibraryMigrationKeepsPapersTogether(t *testing.T) {
	root := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(root, "neoread.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE documents(id TEXT PRIMARY KEY,type TEXT NOT NULL,title TEXT NOT NULL,author TEXT NOT NULL,size INTEGER NOT NULL,created_at TEXT NOT NULL,last_opened_at TEXT NOT NULL,favorite INTEGER NOT NULL DEFAULT 0,progress TEXT,percentage REAL NOT NULL DEFAULT 0,category TEXT NOT NULL DEFAULT 'article',category_source TEXT NOT NULL DEFAULT 'default',classification_status TEXT NOT NULL DEFAULT 'pending',classification_error TEXT NOT NULL DEFAULT '',tags TEXT NOT NULL DEFAULT '[]');
 INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at,category) VALUES('paper','pdf','Paper','',100,'t','t','paper'),('huge','pdf','Huge','',99999999,'t','t','paper'),('article','pdf','Article','',100,'t','t','article'),('book','epub','Book','',100,'t','t','book');`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	want := map[string]string{"paper": "papers", "huge": "books", "article": "books", "book": "books"}
	for id, library := range want {
		if d, err := store.Document(id); err != nil || d.Library != library {
			t.Fatalf("%s: %+v %v", id, d, err)
		}
	}
}
func TestLibraryPreferences(t *testing.T) {
	s := testServer(t)
	if w := request(t, s, "GET", "/api/preferences/library", nil); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("empty: %d %s", w.Code, w.Body.String())
	}
	if w := request(t, s, "PUT", "/api/preferences/library", strings.NewReader(`{"mode":"papers","pinned":["tag:ML"]}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w := request(t, s, "GET", "/api/preferences/library", nil)
	if !strings.Contains(w.Body.String(), `"mode":"papers"`) {
		t.Fatal(w.Body.String())
	}
	if w = request(t, s, "PUT", "/api/preferences/guide", strings.NewReader(`{"onboarded":true,"seenVersion":"0.3.0"}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request(t, s, "GET", "/api/preferences/guide", nil); !strings.Contains(w.Body.String(), `"seenVersion":"0.3.0"`) {
		t.Fatal(w.Body.String())
	}
	if w = request(t, s, "GET", "/api/preferences/library", nil); strings.Contains(w.Body.String(), "seenVersion") {
		t.Fatalf("keys share a value: %s", w.Body.String())
	}
	if w = request(t, s, "PUT", "/api/preferences/other", strings.NewReader(`{}`)); w.Code != 404 {
		t.Fatalf("unknown key: %d", w.Code)
	}
	if w = request(t, s, "PUT", "/api/preferences/library", strings.NewReader(`[1]`)); w.Code != 400 {
		t.Fatalf("non-object: %d", w.Code)
	}
}
func TestRenameAndRemoveLibraryTag(t *testing.T) {
	s := testServer(t)
	ids := []string{}
	for i, library := range []string{"papers", "papers", "books"} {
		w := uploadTo(t, s, library, fmt.Sprintf("p%d.pdf", i), testPDF(i+1))
		var d Document
		_ = json.Unmarshal(w.Body.Bytes(), &d)
		ids = append(ids, d.ID)
	}
	request(t, s, "PATCH", "/api/documents/"+ids[0], strings.NewReader(`{"tags":["ML","Vision"]}`))
	request(t, s, "PATCH", "/api/documents/"+ids[1], strings.NewReader(`{"tags":["ml","NLP"]}`))
	request(t, s, "PATCH", "/api/documents/"+ids[2], strings.NewReader(`{"tags":["ML"]}`))
	request(t, s, "POST", "/api/documents/"+ids[1]+"/trash", nil)
	w := request(t, s, "POST", "/api/libraries/papers/tags", strings.NewReader(`{"from":"ML","to":"NLP"}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"changed":2`) {
		t.Fatal(w.Body.String())
	}
	a, _ := s.Store.Document(ids[0])
	b, _ := s.Store.Document(ids[1])
	c, _ := s.Store.Document(ids[2])
	if strings.Join(a.Tags, ",") != "NLP,Vision" || strings.Join(b.Tags, ",") != "NLP" || strings.Join(c.Tags, ",") != "ML" {
		t.Fatalf("rename: %v %v %v", a.Tags, b.Tags, c.Tags)
	}
	if w = request(t, s, "POST", "/api/libraries/papers/tags", strings.NewReader(`{"from":"NLP"}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	a, _ = s.Store.Document(ids[0])
	if strings.Join(a.Tags, ",") != "Vision" {
		t.Fatalf("remove: %v", a.Tags)
	}
	if w = request(t, s, "POST", "/api/libraries/papers/tags", strings.NewReader(`{"from":"Vision","to":" "}`)); w.Code != 400 {
		t.Fatalf("blank rename: %d", w.Code)
	}
	if w = request(t, s, "POST", "/api/libraries/shelf/tags", strings.NewReader(`{"from":"x"}`)); w.Code != 404 {
		t.Fatalf("unknown library: %d", w.Code)
	}
}

func TestFoldersNestButTagsStayFlat(t *testing.T) {
	s := testServer(t)
	w := uploadTo(t, s, "papers", "p.pdf", testPDF(1))
	var d Document
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"folders":["ML/Vision","ml/NLP/Parsing","MLOps"],"tags":["ML","ML/x"]}`))
	if w = request(t, s, "POST", "/api/libraries/papers/folders", strings.NewReader(`{"from":"ML","to":"AI"}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	d, _ = s.Store.Document(d.ID)
	if strings.Join(d.Folders, ",") != "AI/Vision,AI/NLP/Parsing,MLOps" || strings.Join(d.Tags, ",") != "ML,ML/x" {
		t.Fatalf("folder rename: folders=%v tags=%v", d.Folders, d.Tags)
	}
	if w = request(t, s, "POST", "/api/libraries/papers/folders", strings.NewReader(`{"from":"AI/NLP"}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request(t, s, "POST", "/api/libraries/papers/tags", strings.NewReader(`{"from":"ML","to":"Learning"}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	d, _ = s.Store.Document(d.ID)
	if strings.Join(d.Folders, ",") != "AI/Vision,MLOps" || strings.Join(d.Tags, ",") != "Learning,ML/x" {
		t.Fatalf("folders=%v tags=%v", d.Folders, d.Tags)
	}
	long := strings.Repeat("x", 39)
	if w = request(t, s, "POST", "/api/libraries/papers/folders", strings.NewReader(`{"from":"AI","to":"`+long+`"}`)); w.Code != 400 {
		t.Fatalf("too long after rename: %d", w.Code)
	}
	if w = request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"folders":"x"}`)); w.Code != 400 {
		t.Fatalf("bad folders: %d", w.Code)
	}
}

func TestShortcutPreferences(t *testing.T) {
	s := testServer(t)
	if w := request(t, s, "PUT", "/api/preferences/shortcuts", strings.NewReader(`{"readerLeft":{"key":"b","shift":true}}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := request(t, s, "GET", "/api/preferences/shortcuts", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"key":"b"`) {
		t.Fatal(w.Body.String())
	}
	if w := request(t, s, "GET", "/api/preferences/guide", nil); strings.Contains(w.Body.String(), "readerLeft") {
		t.Fatal("shortcut preferences overwrite guide")
	}
}

func migrationSource(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "source")
	s, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(`INSERT INTO settings VALUES('migration-test','notes and settings')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(`INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at,percentage) VALUES('test-document','pdf','Migration paper','Author',9,'now','now',0.5);
 INSERT INTO document_notes VALUES('test-document','Keep my notes','now');
 INSERT INTO annotations VALUES('annotation','test-document','{"text":"Keep my highlight"}');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"papers/paper.pdf": "pdf bytes", "books/book.epub": "epub bytes", "cache/analysis.md": "analysis", "attachments/supplement": "attachment bytes", "server.json": "stale token", "ai-work/transient": "temporary"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
func TestCopyLibraryPreservesSourceAndDatabase(t *testing.T) {
	source := migrationSource(t)
	target := filepath.Join(t.TempDir(), "新书库 # &")
	if err := CopyLibrary(source, target); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{source, target} {
		b, err := os.ReadFile(filepath.Join(root, "papers/paper.pdf"))
		if err != nil || string(b) != "pdf bytes" {
			t.Fatal(string(b), err)
		}
		b, err = os.ReadFile(filepath.Join(root, "attachments/supplement"))
		if err != nil || string(b) != "attachment bytes" {
			t.Fatal("attachment migration", string(b), err)
		}
		s, err := OpenStore(root)
		if err != nil {
			t.Fatal(err)
		}
		var value string
		err = s.DB.QueryRow(`SELECT value FROM settings WHERE key='migration-test'`).Scan(&value)
		var note, annotation string
		var progress float64
		if err := s.DB.QueryRow("SELECT body FROM document_notes WHERE document_id='test-document'").Scan(&note); err != nil {
			t.Fatal(err)
		}
		if err := s.DB.QueryRow("SELECT body FROM annotations WHERE id='annotation'").Scan(&annotation); err != nil {
			t.Fatal(err)
		}
		if err := s.DB.QueryRow("SELECT percentage FROM documents WHERE id='test-document'").Scan(&progress); err != nil {
			t.Fatal(err)
		}
		if note != "Keep my notes" || annotation != `{"text":"Keep my highlight"}` || progress != 0.5 {
			t.Fatal(note, annotation, progress)
		}
		s.DB.Close()
		if err != nil || value != "notes and settings" {
			t.Fatal(value, err)
		}
	}
	for _, name := range []string{"server.json", "ai-work/transient"} {
		if _, err := os.Stat(filepath.Join(target, name)); !os.IsNotExist(err) {
			t.Fatalf("copied ephemeral file %s: %v", name, err)
		}
	}
}
func TestCopyLibraryRefusesExistingAndNestedTargets(t *testing.T) {
	source := migrationSource(t)
	existing := t.TempDir()
	if err := os.WriteFile(filepath.Join(existing, "keep"), []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{source, filepath.Join(source, "nested"), filepath.Dir(source), existing} {
		if err := CopyLibrary(source, target); err == nil {
			t.Fatalf("accepted %s", target)
		}
	}
	b, err := os.ReadFile(filepath.Join(existing, "keep"))
	if err != nil || string(b) != "untouched" {
		t.Fatal(err, string(b))
	}
}

func TestCopyLibraryRejectsQuestionMarkPathsBeforeCopying(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not allow question marks in directory names")
	}
	source := migrationSource(t)
	for _, relative := range []string{"NeoRead?Library", filepath.Join("parent?question", "NeoRead Library")} {
		t.Run(relative, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), relative)
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			if err := CopyLibrary(source, target); err == nil || !strings.Contains(err.Error(), "书库路径不能包含 ?") {
				t.Fatalf("expected path validation error, got %v", err)
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("created unsafe target: %v", err)
			}
		})
	}
	store, err := OpenStore(source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	var note string
	if err := store.DB.QueryRow("SELECT body FROM document_notes WHERE document_id='test-document'").Scan(&note); err != nil || note != "Keep my notes" {
		t.Fatalf("source note changed: %q, %v", note, err)
	}
}

func TestCopyLibraryWaitsForSourceLock(t *testing.T) {
	source := migrationSource(t)
	target := filepath.Join(t.TempDir(), "target")
	unlock, err := LockLibrary(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := CopyLibrary(source, target); err == nil {
		t.Fatal("copied active library")
	}
	unlock()
	if err := CopyLibrary(source, target); err != nil {
		t.Fatal(err)
	}
}
func TestCopyLibraryRejectsLinksAndCleansPartialCopy(t *testing.T) {
	source := migrationSource(t)
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(source, "linked")); err != nil {
		t.Skip(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := CopyLibrary(source, target); err == nil {
		t.Fatal("followed symlink")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("left partial target: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal(err)
	}
}
func TestCopyLibraryRejectsMissingOrInvalidDatabase(t *testing.T) {
	for _, data := range []string{"", "not sqlite"} {
		source := t.TempDir()
		target := filepath.Join(t.TempDir(), "target")
		if data != "" {
			if err := os.WriteFile(filepath.Join(source, "neoread.sqlite"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := CopyLibrary(source, target); err == nil {
			t.Fatal("accepted invalid library")
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatalf("left target: %v", err)
		}
	}
}

func TestCopyLibraryRejectsEmptySQLiteFile(t *testing.T) {
	source := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(filepath.Join(source, "neoread.sqlite"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := CopyLibrary(source, target); err == nil {
		t.Fatal("accepted empty database")
	}
}
func TestCopyLibraryRecoversCommittedWAL(t *testing.T) {
	live := migrationSource(t)
	s, err := OpenStore(live)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE document_notes SET body='Committed in WAL'"); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	for _, name := range []string{"neoread.sqlite", "neoread.sqlite-wal"} {
		data, err := os.ReadFile(filepath.Join(live, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s.DB.Close()
	target := filepath.Join(t.TempDir(), "target")
	if err := CopyLibrary(source, target); err != nil {
		t.Fatal(err)
	}
	recovered, err := OpenStore(target)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.DB.Close()
	var note string
	if err := recovered.DB.QueryRow("SELECT body FROM document_notes WHERE document_id='test-document'").Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note != "Committed in WAL" {
		t.Fatal(note)
	}
}

func TestLibraryDatabaseURI(t *testing.T) {
	for path, want := range map[string]string{
		"C:/NeoRead Library/neoread.sqlite":   "file:///C:/NeoRead%20Library/neoread.sqlite?mode=rw",
		"/tmp/NeoRead Library/neoread.sqlite": "file:///tmp/NeoRead%20Library/neoread.sqlite?mode=rw",
	} {
		if got := libraryDatabaseURI(path); got != want {
			t.Fatalf("%s: got %s, want %s", path, got, want)
		}
	}
}

func TestCopyLibraryPreservesCorpusOverviewAndReferences(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%t", stale), func(t *testing.T) {
			s, a := originalDocumentFixture(t)
			calls, failAt := 0, -1
			if err := s.compileOverview(t.Context(), overviewTestJob(t, s), overviewTestService(t, &calls, &failAt)); err != nil {
				t.Fatal(err)
			}
			reference := map[string]any{"sourceId": "S1", "corpusRevision": a.corpus.Revision, "text": "Original evidence", "contentHash": sourceKey("Original evidence")}
			if _, err := s.Store.DB.Exec("INSERT INTO messages VALUES('answer','doc',?,'now')", jsonText(map[string]any{"role": "assistant", "references": []any{reference}})); err != nil {
				t.Fatal(err)
			}
			if stale {
				if err := os.WriteFile(filepath.Join(s.Store.Root, "cache", "doc", "a.xhtml"), []byte("Changed original content"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			source := s.Store.Root
			s.Close()
			if err := s.Store.DB.Close(); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "moved library")
			if err := CopyLibrary(source, target); err != nil {
				t.Fatal(err)
			}
			for _, root := range []string{source, target} {
				store, err := OpenStore(root)
				if err != nil {
					t.Fatal(err)
				}
				defer store.DB.Close()
				reopened := NewServer(store, "test-secret", "")
				defer reopened.Close()
				overview, err := reopened.overviewState(t.Context(), "doc")
				if err != nil || overview.Stale != stale {
					t.Fatalf("overview at %s: stale=%t want %t, %v", root, overview.Stale, stale, err)
				}
				w := request(t, reopened, "GET", "/api/documents/doc/messages/answer/sources/S1", nil)
				want := 200
				if stale {
					want = 409
				}
				if w.Code != want {
					t.Fatalf("reference at %s: %d want %d: %s", root, w.Code, want, w.Body.String())
				}
				var revision, body string
				if err := store.DB.QueryRow("SELECT revision,body FROM document_corpora WHERE document_id='doc'").Scan(&revision, &body); err != nil {
					t.Fatal(err)
				}
				if revision != a.corpus.Revision || body != jsonText(a.corpus) {
					t.Fatal("corpus content/revision changed")
				}
				if !stale {
					c, err := reopened.documentCorpus(t.Context(), "doc", func() error { return fmt.Errorf("unchanged corpus must not rebuild") })
					if err != nil || c.Revision != a.corpus.Revision {
						t.Fatal("cached corpus not reusable", err)
					}
				}
			}
		})
	}
}

func TestValidateLibraryPreservesDataAndRejectsInvalidLibraries(t *testing.T) {
	root := migrationSource(t)
	before, err := os.ReadFile(filepath.Join(root, "neoread.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateLibrary(root); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, "neoread.sqlite"))
	if err != nil || string(before) != string(after) {
		t.Fatal("validation modified database", err)
	}
	unlock, err := LockLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateLibrary(root); err == nil {
		t.Fatal("accepted an in-use library")
	}
	unlock()
	empty := t.TempDir()
	if err := ValidateLibrary(empty); err == nil {
		t.Fatal("accepted empty directory")
	}
	if _, err := os.Stat(filepath.Join(empty, "neoread.sqlite")); !os.IsNotExist(err) {
		t.Fatal("created a database")
	}
	if err := os.WriteFile(filepath.Join(empty, "neoread.sqlite"), []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLibrary(empty); err == nil {
		t.Fatal("accepted corrupt database")
	}
}

func TestValidateLegacyLibraryWithoutRenamingDatabase(t *testing.T) {
	root := migrationSource(t)
	if err := os.Rename(filepath.Join(root, "neoread.sqlite"), filepath.Join(root, "reader.sqlite")); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLibrary(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "neoread.sqlite")); !os.IsNotExist(err) {
		t.Fatal("validation renamed legacy database")
	}
}

func TestValidateLibraryRejectsDanglingForeignKeys(t *testing.T) {
	for _, filename := range []string{"neoread.sqlite", "reader.sqlite"} {
		t.Run(filename, func(t *testing.T) {
			root := migrationSource(t)
			database := filepath.Join(root, filename)
			if filename != "neoread.sqlite" {
				if err := os.Rename(filepath.Join(root, "neoread.sqlite"), database); err != nil {
					t.Fatal(err)
				}
			}
			db, err := sql.Open("sqlite", libraryDatabaseURI(database))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			defer db.Close()
			if _, err := db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO annotations VALUES('orphan','missing-document','{}')`); err != nil {
				t.Fatal(err)
			}
			var result string
			if err := db.QueryRow("PRAGMA quick_check").Scan(&result); err != nil || result != "ok" {
				t.Fatalf("fixture must pass quick_check: %s, %v", result, err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(database)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateLibrary(root); err == nil || !strings.Contains(err.Error(), "外键") {
				t.Fatalf("expected foreign-key validation error, got %v", err)
			}
			after, err := os.ReadFile(database)
			if err != nil || string(before) != string(after) {
				t.Fatal("validation changed database", err)
			}
		})
	}
}

func TestMergeKeepsZoteroImportedVersion(t *testing.T) {
	for _, scenario := range []string{"new import", "existing file", "legacy import"} {
		t.Run(scenario, func(t *testing.T) {
			dir, _ := zoteroFixture(t)
			s := testServer(t)
			if scenario == "existing file" {
				w := uploadTo(t, s, "papers", "existing.pdf", testPDF(2))
				if w.Code != 201 {
					t.Fatal(w.Body.String())
				}
			}
			imported, err := s.importZoteroEntry(context.Background(), mustScanZotero(t, dir).Entries[0])
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "legacy import" {
				// Older imports bound JSON bytes as BLOBs, even in TEXT columns.
				if _, err := s.Store.DB.Exec(`UPDATE documents SET metadata=CAST(metadata AS BLOB),tags=CAST(tags AS BLOB),folders=CAST(folders AS BLOB) WHERE id=?`, imported.DocumentID); err != nil {
					t.Fatal(err)
				}
				root := s.Store.Root
				s.Close()
				if err := s.Store.DB.Close(); err != nil {
					t.Fatal(err)
				}
				store, err := OpenStore(root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { store.DB.Close() })
				s = NewServer(store, "test-secret", "")
				t.Cleanup(s.Close)
			}
			var metaType, tagsType, foldersType string
			if err := s.Store.DB.QueryRow(`SELECT typeof(metadata),typeof(tags),typeof(folders) FROM documents WHERE id=?`, imported.DocumentID).Scan(&metaType, &tagsType, &foldersType); err != nil {
				t.Fatal(err)
			}
			if metaType != "text" || tagsType != "text" || foldersType != "text" {
				t.Fatalf("imported JSON storage types: %s/%s/%s", metaType, tagsType, foldersType)
			}
			var annotationBefore string
			if err := s.Store.DB.QueryRow(`SELECT body FROM annotations WHERE document_id=?`, imported.DocumentID).Scan(&annotationBefore); err != nil {
				t.Fatal(err)
			}
			duplicate := uploadPapers(t, s, 1)[0]
			if w := request(t, s, "PATCH", "/api/documents/"+duplicate, strings.NewReader(`{"tags":["extra"],"metadata":{"publisher":"Publisher"}}`)); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			if w := request(t, s, "PUT", "/api/documents/"+duplicate+"/note", strings.NewReader(`{"body":"duplicate note"}`)); w.Code != 200 {
				t.Fatal(w.Body.String())
			}

			w := request(t, s, "POST", "/api/documents/"+imported.DocumentID+"/merge", strings.NewReader(`{"from":["`+duplicate+`"]}`))
			if w.Code != 200 {
				t.Fatalf("merge unchanged Zotero document: %d %s", w.Code, w.Body.String())
			}
			var result mergeResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			d := result.Document
			if d.ID != imported.DocumentID || d.Metadata.DOI != "10.1000/example" || d.Metadata.Publisher != "Publisher" || strings.Join(d.Tags, ",") != "research,extra" || strings.Join(d.Folders, ",") != "AI/Security" {
				t.Fatalf("merged document: %+v", d)
			}
			if d.HighlightCount != 1 || len(result.Trashed) != 1 || result.Trashed[0] != duplicate {
				t.Fatalf("merge lost annotations or did not trash duplicate: %+v", result)
			}
			var annotationAfter string
			if err := s.Store.DB.QueryRow(`SELECT body FROM annotations WHERE document_id=?`, d.ID).Scan(&annotationAfter); err != nil {
				t.Fatal(err)
			}
			if annotationBefore != annotationAfter {
				t.Fatal("merge changed the kept version's annotation")
			}
			trash := listTrash(t, s, "papers")
			if len(trash) != 1 || trash[0].ID != duplicate {
				t.Fatalf("duplicate not in trash: %+v", trash)
			}
			note := request(t, s, "GET", "/api/documents/"+d.ID+"/note", nil).Body.String()
			if !strings.Contains(note, "我的笔记") || strings.Contains(note, "duplicate note") {
				t.Fatal(note)
			}
		})
	}
}

func uploadPapers(t *testing.T, s *Server, n int) []string {
	t.Helper()
	ids := []string{}
	for i := range n {
		w := uploadTo(t, s, "papers", fmt.Sprintf("p%d.pdf", i), testPDF(i+1))
		var d Document
		_ = json.Unmarshal(w.Body.Bytes(), &d)
		ids = append(ids, d.ID)
	}
	return ids
}

func TestMergeFoldsDuplicatesIntoTheChosenVersion(t *testing.T) {
	s := testServer(t)
	ids := uploadPapers(t, s, 3)
	request(t, s, "PATCH", "/api/documents/"+ids[0], strings.NewReader(`{"tags":["ML"],"folders":["Reading"],"metadata":{"venue":"NeurIPS"}}`))
	request(t, s, "PATCH", "/api/documents/"+ids[1], strings.NewReader(`{"tags":["ml","NLP"],"folders":["Survey"],"favorite":true,"readingStatus":"done","metadata":{"venue":"arXiv","doi":"10.1000/xyz","creators":[{"given":"Ashish","family":"Vaswani"}]}}`))
	request(t, s, "PUT", "/api/documents/"+ids[1]+"/note", strings.NewReader(`{"body":"preprint note"}`))
	request(t, s, "PUT", "/api/documents/"+ids[0]+"/note", strings.NewReader(`{"body":"main note"}`))
	if _, err := s.Store.DB.Exec(`INSERT INTO annotations VALUES('a1', ?, '{}')`, ids[1]); err != nil {
		t.Fatal(err)
	}

	w := request(t, s, "POST", "/api/documents/"+ids[0]+"/merge", strings.NewReader(`{"from":["`+ids[1]+`"]}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var result mergeResult
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	d := result.Document
	if strings.Join(d.Tags, ",") != "ML,NLP" || strings.Join(d.Folders, ",") != "Reading,Survey" || !d.Favorite || d.ReadingStatus != "done" {
		t.Fatalf("merged: %+v", d)
	}
	m := d.Metadata
	if m.Venue != "NeurIPS" || m.Sources["venue"] != "manual" || m.DOI != "10.1000/xyz" || d.Author != "Ashish Vaswani" {
		t.Fatalf("metadata: %+v author=%q", m, d.Author)
	}
	if len(result.Trashed) != 1 || result.Trashed[0] != ids[1] {
		t.Fatalf("trashed: %v", result.Trashed)
	}
	note := request(t, s, "GET", "/api/documents/"+ids[0]+"/note", nil).Body.String()
	if !strings.Contains(note, "preprint note") || strings.Contains(note, "main note") {
		t.Fatal(note)
	}
	// The duplicate keeps its own annotations in the trash.
	var count int
	s.Store.DB.QueryRow("SELECT count(*) FROM annotations WHERE document_id=?", ids[1]).Scan(&count)
	trash := listTrash(t, s, "papers")
	if count != 1 || len(trash) != 1 || trash[0].ID != ids[1] {
		t.Fatalf("duplicate data: annotations=%d trash=%+v", count, trash)
	}
}

func TestMergeRejectsInvalidRequests(t *testing.T) {
	s := testServer(t)
	ids := uploadPapers(t, s, 2)
	book := uploadTo(t, s, "books", "b.pdf", testPDF(9))
	var b Document
	_ = json.Unmarshal(book.Body.Bytes(), &b)
	cases := map[string]string{
		`{"from":[]}`:                                  "empty",
		`{"from":["` + ids[0] + `"]}`:                  "self",
		`{"from":["` + ids[1] + `","` + ids[1] + `"]}`: "repeat",
		`{"from":["` + b.ID + `"]}`:                    "other library",
	}
	for body, name := range cases {
		if w := request(t, s, "POST", "/api/documents/"+ids[0]+"/merge", strings.NewReader(body)); w.Code != 400 {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	request(t, s, "POST", "/api/documents/"+ids[1]+"/trash", nil)
	if w := request(t, s, "POST", "/api/documents/"+ids[0]+"/merge", strings.NewReader(`{"from":["`+ids[1]+`"]}`)); w.Code != 404 {
		t.Fatalf("trashed duplicate: %d", w.Code)
	}
	if d, _ := s.Store.Document(ids[0]); len(d.Tags) != 0 || d.DeletedAt != "" {
		t.Fatalf("failed merge changed the master: %+v", d)
	}
}

func TestMergeSelectsLongestNoteWithoutNormalizing(t *testing.T) {
	for _, tc := range []struct{ name, master, other, want string }{
		{"longer duplicate", "old", "## New\n\nlonger note\n", "## New\n\nlonger note\n"},
		{"longer master", "a longer master", "short", "a longer master"},
		{"tie keeps master", "first", "other", "first"},
		{"unicode characters not bytes", "中文", "abc", "abc"},
		{"empty duplicate", "keep", "", "keep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t)
			ids := uploadPapers(t, s, 2)
			for i, body := range []string{tc.master, tc.other} {
				data, _ := json.Marshal(map[string]string{"body": body})
				if w := request(t, s, "PUT", "/api/documents/"+ids[i]+"/note", strings.NewReader(string(data))); w.Code != 200 {
					t.Fatal(w.Body.String())
				}
			}
			w := request(t, s, "POST", "/api/documents/"+ids[0]+"/merge", strings.NewReader(`{"from":["`+ids[1]+`"]}`))
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			var actual string
			if err := s.Store.DB.QueryRow("SELECT body FROM document_notes WHERE document_id=?", ids[0]).Scan(&actual); err != nil {
				t.Fatal(err)
			}
			if actual != tc.want {
				t.Fatalf("got %q, want %q", actual, tc.want)
			}
		})
	}
}

func TestLibraryArchivePhysicallyRemovesExcludedDatabaseData(t *testing.T) {
	parent := t.TempDir()
	source := archiveFixture(t, filepath.Join(parent, "source"))
	archiveDocument(t, source, "included", "selected")
	archiveDocument(t, source, "excluded", "other")
	const secret = "archiveprivatekeycanary123456789"
	const excluded = "archiveexcludedtextcanary987654321"
	const included = "archiveincludedtextcanary246813579"
	if _, err := source.DB.Exec(`PRAGMA secure_delete=OFF; INSERT INTO settings VALUES('journal-rank:key',?)`, secret); err != nil {
		t.Fatal(err)
	}
	for _, table := range archiveFTSTables {
		if _, err := source.DB.Exec("UPDATE "+table+" SET content=? WHERE document_id='excluded'", excluded); err != nil {
			t.Fatal(err)
		}
		if _, err := source.DB.Exec("UPDATE "+table+" SET content=? WHERE document_id='included'", included); err != nil {
			t.Fatal(err)
		}
	}
	// The overview FTS has no fixture rows yet.
	if _, err := source.DB.Exec(`INSERT INTO document_overviews_fts VALUES('excluded','revision',?),('included','revision',?)`, excluded, included); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []LibraryArchiveOptions{{}, {Library: "papers", Folder: "selected"}} {
		t.Run("folder="+scope.Folder, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "library.zip")
			if err := ExportLibraryArchive(source.Root, path, scope); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "extracted")
			if _, err := extractLibraryArchive(context.Background(), path, destination); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(destination, "neoread.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(body, []byte(secret)) {
				t.Error("excluded key remains in exported database bytes")
			}
			if scope.Folder != "" && bytes.Contains(body, []byte(excluded)) {
				t.Error("excluded document remains in exported database bytes")
			}
			restored := archiveFixture(t, destination)
			for _, table := range archiveFTSTables {
				var count int
				if err := restored.DB.QueryRow("SELECT count(*) FROM "+table+" WHERE "+table+" MATCH ?", included).Scan(&count); err != nil || count != 1 {
					t.Fatalf("included search content lost from %s: %d %v", table, count, err)
				}
			}
		})
	}
}

func archiveFixture(t *testing.T, root string) *Store {
	t.Helper()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DB.Close() })
	return store
}
func archiveDocument(t *testing.T, store *Store, id, folder string) {
	t.Helper()
	folders, _ := json.Marshal([]string{folder})
	if _, err := store.DB.Exec(`INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at,folders,library) VALUES(?,'pdf',?,'作者',8,'2026-11-02','2026-11-02',?,'papers')`, id, id, string(folders)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Root, "papers", id+".pdf"), []byte("PDF "+id), 0600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(store.Root, "cache", id, "analysis")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "manifest.json"), []byte(`{"blocks":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "figure.png"), []byte("original figure "+id), 0600); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := (&Server{Store: store}).corpusFingerprint(Document{ID: id, Type: "pdf"})
	if err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO annotations VALUES(?,?,?)`, []any{"annotation-" + id, id, `{"id":"annotation-` + id + `","documentId":"` + id + `","kind":"highlight","note":"注释","location":{"type":"pdf","page":1}}`}},
		{`INSERT INTO translations(document_id,block_id,source_hash,status,body) VALUES(?,'formula','hash','complete',?)`, []any{id, `{"blockId":"formula","sourceHash":"hash","status":"complete","sentences":[],"formulaMarkdown":"$$x=1$$"}`}},
		{`INSERT INTO document_notes VALUES(?,?,?)`, []any{id, "# 笔记\n原样保存", "2026-11-02"}},
		{`INSERT INTO messages VALUES(?,?,?,?)`, []any{"message-" + id, id, `{"role":"assistant","content":"回答"}`, "2026-11-02"}},
		{`INSERT INTO document_corpora VALUES(?,?,?,?)`, []any{id, fingerprint, "revision", `{"passages":[]}`}},
		{`INSERT INTO search_index VALUES(?,'chapter','original text')`, []any{id}},
		{`INSERT INTO document_passages_fts VALUES(?,'revision','block','searchable passage')`, []any{id}},
	}
	for _, statement := range statements {
		if _, err := store.DB.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLibraryArchiveRoundTripAndCategory(t *testing.T) {
	parent := t.TempDir()
	source := archiveFixture(t, filepath.Join(parent, "source"))
	archiveDocument(t, source, "a", "研究/语言")
	archiveDocument(t, source, "b", "其他")
	if _, err := source.DB.Exec("UPDATE documents SET created_at='2020-03-04T05:06:07Z' WHERE id='a'; UPDATE documents SET created_at='2024-08-09T10:11:12.123Z' WHERE id='b'"); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("attachment"))
	attachment := hex.EncodeToString(hash[:])
	if err := os.WriteFile(filepath.Join(source.Root, "attachments", attachment), []byte("attachment"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := source.DB.Exec(`INSERT INTO paper_attachments(id,document_id,kind,title,sha256,created_at) VALUES('attachment','a','file','附件',?,'now')`, attachment); err != nil {
		t.Fatal(err)
	}
	if _, err := source.DB.Exec(`INSERT INTO settings VALUES('preferences:library','{"papers":{"categories":["研究","研究/语言","其他"]}}'),('api:key','PRIVATE_KEY'),('neoread','{"documentTabs":true}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := source.DB.Exec("INSERT INTO settings VALUES(?,?)", epubMetadataMigration, "already-migrated"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.DB.Exec(`UPDATE documents SET tags='["AI"]' WHERE id='a'; UPDATE settings SET value=json_set(value,'$.papers.smartCategories',json('[{"id":"smart-ai","name":"AI","tags":["ai"]}]')) WHERE key='preferences:library'`); err != nil {
		t.Fatal(err)
	}
	source.DB.Close()
	for _, scope := range []LibraryArchiveOptions{{}, {Library: "papers", Folder: "研究"}, {Library: "papers", SmartCategory: "smart-ai"}} {
		name := "all"
		expected := 2
		if scope.Folder != "" {
			name = "category"
			expected = 1
		}
		if scope.SmartCategory != "" {
			name = "smart"
			expected = 1
		}
		archivePath := filepath.Join(parent, name+".zip")
		if err := ExportLibraryArchive(source.Root, archivePath, scope); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(parent, name)
		if err := ImportLibraryArchive(archivePath, destination, ""); err != nil {
			t.Fatal(err)
		}
		restored := archiveFixture(t, destination)
		var count int
		if err := restored.DB.QueryRow("SELECT count(*) FROM documents").Scan(&count); err != nil || count != expected {
			t.Fatalf("documents: %d %v", count, err)
		}
		for _, table := range []string{"annotations", "translations", "document_notes", "messages", "document_corpora", "search_index", "document_passages_fts"} {
			if err := restored.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != expected {
				t.Fatalf("%s: %d %v", table, count, err)
			}
		}
		var body string
		for id, date := range map[string]string{"a": "2020-03-04T05:06:07Z", "b": "2024-08-09T10:11:12.123Z"} {
			if id == "b" && expected == 1 {
				continue
			}
			var added string
			if err := restored.DB.QueryRow("SELECT created_at FROM documents WHERE id=?", id).Scan(&added); err != nil || added != date {
				t.Fatalf("original added date lost: %s %s %v", id, added, err)
			}
		}
		if err := restored.DB.QueryRow("SELECT body FROM translations WHERE document_id='a'").Scan(&body); err != nil || !strings.Contains(body, "$$x=1$$") {
			t.Fatalf("formula lost: %s %v", body, err)
		}
		data, err := os.ReadFile(filepath.Join(destination, "cache", "a", "analysis", "figure.png"))
		if err != nil || string(data) != "original figure a" {
			t.Fatalf("cache lost: %v", err)
		}
		data, err = os.ReadFile(filepath.Join(destination, "attachments", attachment))
		if err != nil || string(data) != "attachment" {
			t.Fatalf("attachment lost: %v", err)
		}
		if err := restored.DB.QueryRow("SELECT count(*) FROM settings WHERE key='api:key'").Scan(&count); err != nil || count != 0 {
			t.Fatal("exported API key")
		}
		var migrated string
		if err := restored.DB.QueryRow("SELECT value FROM settings WHERE key=?", epubMetadataMigration).Scan(&migrated); err != nil || migrated != "already-migrated" {
			t.Fatal("lost library migration state")
		}
		var fingerprint string
		if err := restored.DB.QueryRow("SELECT fingerprint FROM document_corpora WHERE document_id='a'").Scan(&fingerprint); err != nil {
			t.Fatal(err)
		}
		expectedFingerprint, err := (&Server{Store: restored}).corpusFingerprint(Document{ID: "a", Type: "pdf"})
		if err != nil || fingerprint != expectedFingerprint {
			t.Fatal("corpus index was not rebound")
		}
		if scope.Folder != "" || scope.SmartCategory != "" {
			if _, err := os.Stat(filepath.Join(destination, "papers", "b.pdf")); !os.IsNotExist(err) {
				t.Fatal("unselected original exported")
			}
		}
		restored.DB.Close()
	}
}

func TestLibraryArchiveRejectsUnsafeOrIncompleteInput(t *testing.T) {
	for _, name := range []string{"../outside", "library/cache/../../outside", "library\\outside", "library/cache/link", "library/neoread.sqlite"} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			path := filepath.Join(parent, "bad.zip")
			out, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			archive := zip.NewWriter(out)
			manifest, _ := archive.Create("manifest.json")
			manifest.Write([]byte(`{"format":"neoread-library","version":1,"documents":1}`))
			header := &zip.FileHeader{Name: name, Method: zip.Deflate}
			if strings.HasSuffix(name, "link") {
				header.SetMode(os.ModeSymlink | 0600)
			}
			entry, err := archive.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			entry.Write([]byte("invalid database"))
			archive.Close()
			out.Close()
			destination := filepath.Join(parent, "restored")
			if err := ImportLibraryArchive(path, destination, ""); err == nil {
				t.Fatal("accepted invalid archive")
			}
			if _, err := os.Stat(destination); !os.IsNotExist(err) {
				t.Fatal("published incomplete library")
			}
			if _, err := os.Stat(filepath.Join(parent, "outside")); !os.IsNotExist(err) {
				t.Fatal("escaped extraction directory")
			}
		})
	}
}

func TestLibraryArchiveOnlineImportKeepsSourceOpenAndMergeSettings(t *testing.T) {
	s := testServer(t)
	archiveDocument(t, s.Store, "existing", "原分类")
	config := AIConfig{Primary: "api", Models: map[string]string{"api": "local-model"}, TranslationModels: map[string]string{"api": "local-translation-model"}, API: APIConnection{URL: "https://example.test/api", Key: "LOCAL_CONFIG_ONLY"}}
	if err := s.writeAIConfig(config); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec("INSERT INTO settings(key,value) VALUES('api:key','LOCAL_ONLY')"); err != nil {
		t.Fatal(err)
	}
	incoming := archiveFixture(t, filepath.Join(t.TempDir(), "incoming"))
	archiveDocument(t, incoming, "incoming", "导入分类")
	if err := (&Server{Store: incoming}).writeAIConfig(AIConfig{Primary: "api", API: APIConnection{Key: "DO_NOT_EXPORT_CONFIG"}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "incoming.zip")
	incoming.DB.Close()
	if err := ExportLibraryArchive(incoming.Root, path, LibraryArchiveOptions{}); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockLibrary(s.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, merge := range []bool{false, true} {
		destination := filepath.Join(t.TempDir(), "prepared")
		body, _ := json.Marshal(map[string]any{"file": path, "destination": destination, "merge": merge})
		response := request(t, s, "POST", "/api/library/archive/import", bytes.NewReader(body))
		if response.Code != 200 {
			t.Fatalf("import %t: %d %s", merge, response.Code, response.Body.String())
		}
		restored := archiveFixture(t, destination)
		docs, err := restored.Documents()
		want := 1
		if merge {
			want = 2
		}
		if err != nil || len(docs) != want {
			t.Fatalf("prepared docs %d %v", len(docs), err)
		}
		if merge {
			var secret string
			if err = restored.DB.QueryRow("SELECT value FROM settings WHERE key='api:key'").Scan(&secret); err != nil || secret != "LOCAL_ONLY" {
				t.Fatal("local merge lost machine settings")
			}
			mergedConfig := (&Server{Store: restored}).readAIConfig()
			if mergedConfig.Primary != config.Primary || mergedConfig.API.Key != config.API.Key || mergedConfig.Models["api"] != config.Models["api"] || mergedConfig.TranslationModels["api"] != config.TranslationModels["api"] {
				t.Fatal("local merge lost AI connections or model choices")
			}
		} else if _, err := os.Stat(filepath.Join(destination, "ai-connections.json")); !os.IsNotExist(err) {
			t.Fatal("archive leaked machine AI connections", err)
		}
		if _, err = s.Store.DB.Exec("UPDATE documents SET title='Still live' WHERE id='existing'"); err != nil {
			t.Fatal(err)
		}
	}
}

func chatArchiveSnapshot(t *testing.T, store *Store) map[string][]string {
	t.Helper()
	queries := map[string]string{
		"sessions":      "SELECT json_array(id,document_id,title,provider,created_at,updated_at) FROM chat_sessions WHERE document_id='doc' ORDER BY rowid",
		"messages":      "SELECT json_array(id,body,created_at) FROM messages WHERE document_id='doc' ORDER BY rowid",
		"runs":          "SELECT json_array(id,body) FROM chat_runs WHERE session_id IN (SELECT id FROM chat_sessions WHERE document_id='doc') ORDER BY rowid",
		"evidence":      "SELECT json_array(id,body) FROM chat_agent_runs WHERE document_id='doc' ORDER BY rowid",
		"annotations":   "SELECT body FROM annotations WHERE document_id='doc' ORDER BY rowid",
		"continuations": "SELECT json_array(provider,body) FROM document_conversations WHERE document_id='doc' AND provider!='codex' AND substr(provider,-6)!=':codex' ORDER BY provider",
	}
	result := map[string][]string{}
	for name, query := range queries {
		rows, err := store.DB.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if name == "annotations" {
				// OpenStore normalizes empty optional annotation fields.
				var annotation Annotation
				if err := json.Unmarshal([]byte(raw), &annotation); err != nil {
					t.Fatal(err)
				}
				raw = jsonText(annotation)
			}
			result[name] = append(result[name], raw)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return result
}

func TestChatSessionsPersistAndMigrateAfterEditsAndWithdrawals(t *testing.T) {
	s, a, b, _ := chatEditFixture(t)
	createSessionTest(t, s) // Explicitly retain an empty session; listing no longer creates one.
	if _, err := s.Store.DB.Exec("UPDATE chat_sessions SET title=CASE id WHEN ? THEN '公式讨论' WHEN ? THEN '方法讨论' ELSE title END,provider=CASE id WHEN ? THEN 'codex' ELSE provider END", a.ID, b.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	// The retained second session has a completed run, tool use, web sources and
	// linked answer evidence as well as the first session's source/image context.
	var answer Message
	var raw string
	if err := s.Store.DB.QueryRow("SELECT body FROM messages WHERE id='other-assistant'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(raw), &answer)
	answer.Reasoning = "保留的思考记录"
	answer.References = json.RawMessage(`[{"text":"原文证据","location":{"type":"pdf","page":2},"sourceId":"S1","contentHash":"source-hash","corpusRevision":"revision"}]`)
	answer.Tools = []ToolActivity{{ID: "tool", Kind: "tool", Tool: "read_passage", Title: "读取原文", Status: "completed", StartedAt: answer.CreatedAt, FinishedAt: answer.CreatedAt}}
	answer.WebSources = []WebSource{{ID: "W1", URL: "https://example.com/source", Title: "参考来源", Text: "来源摘录", Kind: "page", RetrievedAt: "2021-02-03T04:05:06Z"}}
	if _, err := s.Store.DB.Exec("UPDATE messages SET body=? WHERE id=?", jsonText(answer), answer.ID); err != nil {
		t.Fatal(err)
	}
	run := chatRunRecord{ChatRun: ChatRun{ID: "other-run", SessionID: b.ID, DocumentID: "doc", UserMessageID: "other-user", Provider: "codex", Status: "completed", StartedAt: answer.CreatedAt, FinishedAt: answer.CreatedAt}, Events: []chatRunEvent{{Event: "delta", Data: json.RawMessage(`{"text":"other session"}`)}, {Event: "done", Data: json.RawMessage(`{"ok":true}`)}}}
	if _, err := s.Store.DB.Exec("INSERT INTO chat_runs VALUES(?,?,?)", run.ID, b.ID, jsonText(run)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec("INSERT INTO chat_agent_runs VALUES(?,?,?)", "other-evidence", "doc", jsonText(map[string]any{"messageId": answer.ID, "tools": answer.Tools, "evidence": json.RawMessage(answer.References), "status": "complete"})); err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join("cache", "doc", "analysis", "assets", "p2-b1.png")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(s.Store.Root, asset)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Store.Root, asset), []byte("original attachment pixels"), 0600); err != nil {
		t.Fatal(err)
	}
	if out := changeQuestionTest(t, s, a.ID, "last-user", "DELETE", chatMessageChange{ExpectedLastMessageID: "last-answer", ExpectedContent: "last"}); out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	content := "编辑后的保留问题 $H'$"
	if out := changeQuestionTest(t, s, a.ID, "middle-user", "PATCH", chatMessageChange{ExpectedLastMessageID: "middle-answer", ExpectedContent: "middle", Content: &content}); out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	want := chatArchiveSnapshot(t, s.Store)
	if len(want["sessions"]) != 3 || len(want["messages"]) != 5 || len(want["runs"]) != 2 || len(want["evidence"]) != 2 {
		t.Fatal("fixture did not exercise multiple persisted sessions", want)
	}
	s.Store.DB.Close()
	reopened := archiveFixture(t, s.Store.Root)
	if got := chatArchiveSnapshot(t, reopened); !reflect.DeepEqual(want, got) {
		t.Fatal("chat records changed after reopening the library", got)
	}
	reopened.DB.Close()
	parent := t.TempDir()
	archivePath := filepath.Join(parent, "聊天书库.zip")
	if err := ExportLibraryArchive(s.Store.Root, archivePath, LibraryArchiveOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"new", "merge"} {
		t.Run(mode, func(t *testing.T) {
			mergeRoot := ""
			if mode == "merge" {
				current := archiveFixture(t, filepath.Join(parent, "existing"))
				archiveDocument(t, current, "existing", "旧分类")
				archiveDocument(t, current, "doc", "旧文档版本")
				if _, err := current.DB.Exec("INSERT INTO chat_sessions VALUES('obsolete-session','doc','被替换的旧会话','api','old','old')"); err != nil {
					t.Fatal(err)
				}
				current.DB.Close()
				mergeRoot = current.Root
			}
			destination := filepath.Join(parent, mode)
			if err := ImportLibraryArchive(archivePath, destination, mergeRoot); err != nil {
				t.Fatal(err)
			}
			restored := archiveFixture(t, destination)
			got := chatArchiveSnapshot(t, restored)
			for table, expected := range want {
				if !reflect.DeepEqual(expected, got[table]) {
					t.Fatalf("%s changed during %s ZIP restore:\nwant %v\ngot %v", table, mode, expected, got[table])
				}
			}
			var count int
			if err := restored.DB.QueryRow("SELECT count(*) FROM document_conversations WHERE provider='codex' OR substr(provider,-6)=':codex'").Scan(&count); err != nil || count != 0 {
				t.Fatal("import retained a machine-bound Codex thread", count, err)
			}
			messages, err := restored.chatHistory("doc", a.ID)
			if err != nil || len(messages) != 3 || messages[2].Content != content || len(messages[2].Attachments) != 1 || len(messages[2].References) == 0 || string(messages[2].ReadingLocation) != `{"type":"pdf","page":2}` {
				t.Fatal("edited question lost its context", messages, err)
			}
			image, err := os.ReadFile(filepath.Join(destination, asset))
			if err != nil || string(image) != "original attachment pixels" {
				t.Fatal("chat image attachment lost", err)
			}
			if mode == "merge" {
				if _, err := restored.Document("existing"); err != nil {
					t.Fatal("merge lost an existing document", err)
				}
			}
		})
	}
}

func TestLibraryArchivePreservesPeakAndCurrentReadingPosition(t *testing.T) {
	store := archiveFixture(t, filepath.Join(t.TempDir(), "source"))
	archiveDocument(t, store, "paper", "Research")
	if _, err := store.DB.Exec(`UPDATE documents SET body_read_position=6.7,progress='{"type":"pdf","page":1}',percentage=.1 WHERE id='paper'`); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "library.zip")
	if err := ExportLibraryArchive(store.Root, archive, LibraryArchiveOptions{}); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	if err := ImportLibraryArchive(archive, destination, ""); err != nil {
		t.Fatal(err)
	}
	restored := archiveFixture(t, destination)
	doc, err := restored.Document("paper")
	if err != nil || doc.BodyReadPosition != 6.7 || !strings.Contains(string(doc.Progress), `"page":1`) {
		t.Fatalf("restored peak or location changed: %+v %v", doc, err)
	}
}

func TestLibraryArchivePreservesLibraryChatsAndExcludesOtherCategorySources(t *testing.T) {
	s := testServer(t)
	archiveDocument(t, s.Store, "alpha", "研究")
	archiveDocument(t, s.Store, "beta", "其他")
	inside := createLibrarySessionTest(t, s, "alpha")
	outside := createLibrarySessionTest(t, s, "alpha", "beta")
	for _, session := range []libraryChatSession{inside, outside} {
		if err := s.Store.saveMessage(Message{ID: id(), DocumentID: libraryChatOwner, SessionID: session.ID, Role: "user", Content: "review " + session.ID}); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(t.TempDir(), "library.zip")
	if err := ExportLibraryArchive(s.Store.Root, archive, LibraryArchiveOptions{}); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "imported")
	if err := ImportLibraryArchive(archive, destination, ""); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(destination)
	if err != nil {
		t.Fatal(err)
	}
	history, err := store.chatHistory(libraryChatOwner, outside.ID)
	if err != nil || len(history) != 1 {
		t.Fatal("whole library export lost review chats", history, err)
	}
	if err = store.saveMessage(Message{ID: id(), DocumentID: libraryChatOwner, SessionID: outside.ID, Role: "user", Content: "withdrawn after export"}); err != nil {
		t.Fatal(err)
	}
	store.DB.Close()
	merged := filepath.Join(t.TempDir(), "merged")
	if err = ImportLibraryArchive(archive, merged, destination); err != nil {
		t.Fatal(err)
	}
	store = archiveFixture(t, merged)
	history, err = store.chatHistory(libraryChatOwner, outside.ID)
	if err != nil || len(history) != 1 {
		t.Fatal("archive merge mixed different transcript versions", history, err)
	}
	store.DB.Close()
	archive = filepath.Join(t.TempDir(), "category.zip")
	if err = ExportLibraryArchive(s.Store.Root, archive, LibraryArchiveOptions{Library: "papers", Folder: "研究"}); err != nil {
		t.Fatal(err)
	}
	destination = filepath.Join(t.TempDir(), "category")
	if err = ImportLibraryArchive(archive, destination, ""); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	history, err = store.chatHistory(libraryChatOwner, inside.ID)
	if err != nil || len(history) != 1 {
		t.Fatal("category export lost its review chat", history, err)
	}
	if _, err = store.libraryScope(outside.ID); err == nil {
		t.Fatal("category export included a chat quoting excluded papers")
	}
	history, err = store.chatHistory(libraryChatOwner, outside.ID)
	if err != nil || len(history) != 0 {
		t.Fatal("category export leaked excluded quotations", history, err)
	}
}

func TestLibraryArchiveDetachesInterruptedSyncOnlyInExportedCopy(t *testing.T) {
	store := archiveFixture(t, filepath.Join(t.TempDir(), "source"))
	archiveDocument(t, store, "paper", "研究")
	if err := os.RemoveAll(filepath.Join(store.Root, "cache", "paper")); err != nil {
		t.Fatal(err)
	}
	pending := syncReceiving{Connection: strings.Repeat("a", 64), Remote: emptySyncDocument(), Baseline: emptySyncDocument(), Clock: syncClock{}, Parents: syncClock{}}
	if _, err := store.DB.Exec("INSERT INTO sync_receiving VALUES(?,?)", "document/paper", jsonText(pending)); err != nil {
		t.Fatal(err)
	}
	processing := initialDocumentProcessing(Document{ID: "paper", Type: "pdf", Library: "papers"}, `{"autoTranslateMaxPages":0}`)
	processing.Receiving, processing.Enabled, processing.Status = true, false, "paused"
	if _, err := store.DB.Exec("INSERT INTO document_processing VALUES(?,?,?,?)", "paper", processing.Phase, processing.Status, jsonText(processing)); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "library.zip")
	if err := ExportLibraryArchive(store.Root, archive, LibraryArchiveOptions{}); err != nil {
		t.Fatal(err)
	}
	if pending, err := readSyncReceiving(t.Context(), store.DB); err != nil || len(pending) != 1 {
		t.Fatal("export changed the source receive state", pending, err)
	}
	destination := filepath.Join(t.TempDir(), "imported")
	if err := ImportLibraryArchive(archive, destination, ""); err != nil {
		t.Fatal(err)
	}
	restored := archiveFixture(t, destination)
	if pending, err := readSyncReceiving(t.Context(), restored.DB); err != nil || len(pending) != 0 {
		t.Fatal("import kept a source-device receive marker", pending, err)
	}
	processing, err := restored.processing("paper")
	if err != nil || processing.Receiving || processing.Status == "paused" || processing.AutoTranslateMaxPages == nil || *processing.AutoTranslateMaxPages != 0 {
		t.Fatal("imported paper stayed blocked or enabled paid translation", processing, err)
	}
}

func TestLibraryArchiveExportsUnopenedLegacyLibraryWithoutChangingItsSchema(t *testing.T) {
	store := archiveFixture(t, filepath.Join(t.TempDir(), "source"))
	archiveDocument(t, store, "paper", "研究")
	for _, table := range []string{"library_chat_runs", "library_messages", "library_document_conversations", "library_chat_agent_runs", "library_processing_usage", "library_chat_usage_coverage", "library_chat_sessions", "sync_receiving", "sync_receipts"} {
		if _, err := store.DB.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(t.TempDir(), "library.zip")
	if err := ExportLibraryArchive(store.Root, archive, LibraryArchiveOptions{}); err != nil {
		t.Fatal(err)
	}
	var tables int
	if err := store.DB.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name='sync_receiving'").Scan(&tables); err != nil || tables != 0 {
		t.Fatal("export migrated the original library", tables, err)
	}
	destination := filepath.Join(t.TempDir(), "imported")
	if err := ImportLibraryArchive(archive, destination, ""); err != nil {
		t.Fatal(err)
	}
	restored := archiveFixture(t, destination)
	if _, err := restored.Document("paper"); err != nil {
		t.Fatal(err)
	}
}
