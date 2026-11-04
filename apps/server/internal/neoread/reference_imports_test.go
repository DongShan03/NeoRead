package neoread

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime/multipart"
	"neoread.local/server/internal/diagnostics"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Synthetic Zotero 6+ schema, using upstream table/column names. No personal
// Zotero library is needed or modified by these tests.
func zoteroFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "zotero.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`
 CREATE TABLE libraries(libraryID INTEGER PRIMARY KEY,type TEXT); INSERT INTO libraries VALUES(1,'user');
 CREATE TABLE itemTypes(itemTypeID INTEGER PRIMARY KEY,typeName TEXT); INSERT INTO itemTypes VALUES(1,'journalArticle'),(2,'attachment'),(3,'note'),(4,'annotation');
 CREATE TABLE items(itemID INTEGER PRIMARY KEY,itemTypeID INT,libraryID INT,key TEXT,dateAdded TEXT);
 INSERT INTO items VALUES(1,1,1,'PAPER001','2024-01-02 03:04:05'),(2,2,1,'PDF00001','2024-01-02 03:04:05'),(3,3,1,'NOTE0001','2024-01-02 03:04:05'),(4,4,1,'ANNOT001','2024-01-02 03:04:05'),(5,4,1,'ANNOT002','2024-01-02 03:04:05');
 CREATE TABLE fields(fieldID INT,fieldName TEXT); INSERT INTO fields VALUES(1,'title'),(2,'DOI'),(3,'publicationTitle'),(4,'date');
 CREATE TABLE itemDataValues(valueID INT,value TEXT); INSERT INTO itemDataValues VALUES(1,'迁移论文'),(2,'10.1000/example'),(3,'Journal'),(4,'2024');
 CREATE TABLE itemData(itemID INT,fieldID INT,valueID INT); INSERT INTO itemData VALUES(1,1,1),(1,2,2),(1,3,3),(1,4,4);
 CREATE TABLE creators(creatorID INT,firstName TEXT,lastName TEXT,fieldMode INT); INSERT INTO creators VALUES(1,'Ada','Lovelace',0),(2,'','研究小组',1);
 CREATE TABLE creatorTypes(creatorTypeID INT,creatorType TEXT); INSERT INTO creatorTypes VALUES(1,'author');
 CREATE TABLE itemCreators(itemID INT,creatorID INT,creatorTypeID INT,orderIndex INT); INSERT INTO itemCreators VALUES(1,1,1,0),(1,2,1,1);
 CREATE TABLE collections(collectionID INT,collectionName TEXT,parentCollectionID INT); INSERT INTO collections VALUES(1,'AI',NULL),(2,'Security',1);
 CREATE TABLE collectionItems(collectionID INT,itemID INT); INSERT INTO collectionItems VALUES(2,1);
 CREATE TABLE tags(tagID INT,name TEXT); INSERT INTO tags VALUES(1,'research'),(2,'重要');
 CREATE TABLE itemTags(itemID INT,tagID INT); INSERT INTO itemTags VALUES(1,1),(4,2);
 CREATE TABLE itemNotes(itemID INT,parentItemID INT,note TEXT); INSERT INTO itemNotes VALUES(3,1,'<div><p>我的笔记 &amp; 证据</p><script>evil()</script><p>第二段</p></div>');
 CREATE TABLE itemAttachments(itemID INT,parentItemID INT,path TEXT,linkMode INT,contentType TEXT); INSERT INTO itemAttachments VALUES(2,1,'storage:paper.pdf',0,'application/pdf');
 CREATE TABLE itemAnnotations(itemID INT,parentItemID INT,type INT,text TEXT,comment TEXT,color TEXT,position TEXT,sortIndex TEXT);
 INSERT INTO itemAnnotations VALUES(4,2,1,'原文','批注','#ffd400','{"pageIndex":0,"rects":[[61.2,633.6,306,712.8]]}','00001'),(5,2,4,'','手绘','invalid','{"pageIndex":0,"paths":[[1,2,3,4]]}','00002');
 CREATE TABLE deletedItems(itemID INT);
 `)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "storage", "PDF00001")
	if err = os.MkdirAll(dest, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dest, "paper.pdf"), testPDF(2), 0600); err != nil {
		t.Fatal(err)
	}
	return dir, db
}
func mustScanZotero(t *testing.T, dir string) *zoteroScan {
	t.Helper()
	scan, err := readZotero(context.Background(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	return scan
}
func TestZoteroMigrationRoundTrip(t *testing.T) {
	dir, db := zoteroFixture(t)
	s := testServer(t)
	before, err := os.ReadFile(filepath.Join(dir, "zotero.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	scan := mustScanZotero(t, dir)
	if len(scan.Entries) != 1 {
		t.Fatalf("entries: %+v", scan.Entries)
	}
	entry := scan.Entries[0]
	if entry.Issue != "" || entry.Notes != 1 || entry.Annotations != 2 || entry.Title != "迁移论文" || strings.Join(entry.Collections, ",") != "AI/Security" {
		t.Fatalf("entry: %+v", entry)
	}
	result, err := s.importZoteroEntry(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "imported" || result.Annotations != 1 || result.Notes != 2 || len(result.Warnings) == 0 {
		t.Fatalf("result: %+v", result)
	}
	doc, err := s.Store.Document(result.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	if doc.LastOpenedAt != "" || doc.Title != "迁移论文" || doc.Metadata.DOI != "10.1000/example" || doc.Metadata.Sources["doi"] != "manual" || doc.CreatedAt != "2024-01-02T03:04:05Z" || doc.Author != "Ada Lovelace, 研究小组" || len(doc.Tags) != 1 || len(doc.Folders) != 1 {
		t.Fatalf("document: %+v", doc)
	}
	imported, err := os.ReadFile(s.Store.File(doc))
	if err != nil || !bytes.Equal(imported, testPDF(2)) {
		t.Fatal("PDF not copied intact", err)
	}
	var note string
	err = s.Store.DB.QueryRow(`SELECT body FROM document_notes WHERE document_id=?`, doc.ID).Scan(&note)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "我的笔记 & 证据") || strings.Contains(note, "evil()") || !strings.Contains(note, "手绘") {
		t.Fatal(note)
	}
	var annotationBody string
	err = s.Store.DB.QueryRow(`SELECT body FROM annotations WHERE document_id=?`, doc.ID).Scan(&annotationBody)
	if err != nil {
		t.Fatal(err)
	}
	var annotation Annotation
	if err = json.Unmarshal([]byte(annotationBody), &annotation); err != nil {
		t.Fatal(err)
	}
	var location annotationPDFLocation
	if err = json.Unmarshal(annotation.Location, &location); err != nil {
		t.Fatal(err)
	}
	if location.Page != 1 || len(location.Rects) != 1 || math.Abs(location.Rects[0].Y-.1) > 1e-8 || annotation.Tags[0] != "重要" {
		t.Fatalf("annotation: %+v %+v", annotation, location)
	}
	var processingBody string
	err = s.Store.DB.QueryRow(`SELECT body FROM document_processing WHERE document_id=?`, doc.ID).Scan(&processingBody)
	if err != nil {
		t.Fatal(err)
	}
	var p Processing
	json.Unmarshal([]byte(processingBody), &p)
	if !p.Enabled || p.AutoTranslateMaxPages == nil || *p.AutoTranslateMaxPages != 50 {
		t.Fatal("migration must use the default translation threshold")
	}
	// Curated NeoRead data, including removed imported annotations, survives a retry.
	if _, err = s.Store.DB.Exec(`UPDATE document_notes SET body='NeoRead 编辑' WHERE document_id=?; DELETE FROM annotations WHERE document_id=?`, doc.ID, doc.ID); err != nil {
		t.Fatal(err)
	}
	again, err := s.importZoteroEntry(context.Background(), mustScanZotero(t, dir).Entries[0])
	if err != nil || again.Status != "exists" {
		t.Fatalf("retry: %+v %v", again, err)
	}
	var count int
	s.Store.DB.QueryRow(`SELECT count(*) FROM annotations`).Scan(&count)
	if count != 0 {
		t.Fatal("retry resurrected deleted annotation")
	}
	s.Store.DB.QueryRow(`SELECT body FROM document_notes WHERE document_id=?`, doc.ID).Scan(&note)
	if note != "NeoRead 编辑" {
		t.Fatal("retry overwrote notes")
	}
	after, err := os.ReadFile(filepath.Join(dir, "zotero.sqlite"))
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("Zotero database changed")
	}
	// Local read-only access also sees committed WAL data.
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; INSERT INTO items VALUES(10,1,1,'NOPDF001','2024-01-01')`); err != nil {
		t.Fatal(err)
	}
	if len(mustScanZotero(t, dir).Entries) != 2 {
		t.Fatal("WAL records not visible")
	}
}
func TestZoteroMissingDeletedAndLinkedAttachments(t *testing.T) {
	dir, db := zoteroFixture(t)
	if _, err := db.Exec(`INSERT INTO items VALUES(6,1,1,'DELETED1','2024'),(7,2,1,'MISSING1','2024'),(8,1,1,'NOPDF001','2024'); INSERT INTO deletedItems VALUES(6); INSERT INTO itemAttachments VALUES(7,1,'storage:missing.pdf',0,'application/pdf')`); err != nil {
		t.Fatal(err)
	}
	scan := mustScanZotero(t, dir)
	if len(scan.Entries) != 3 || scan.Entries[1].Issue == "" || scan.Entries[2].Issue == "" {
		t.Fatalf("missing entries: %+v", scan.Entries)
	}
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "linked.pdf"), testPDF(1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE itemAttachments SET path='attachments:linked.pdf',linkMode=2 WHERE itemID=2`); err != nil {
		t.Fatal(err)
	}
	if mustScanZotero(t, dir).Entries[0].Issue == "" {
		t.Fatal("relative attachment accepted without base")
	}
	linked, err := readZotero(context.Background(), dir, base)
	if err != nil || linked.Entries[0].Issue != "" {
		t.Fatalf("linked: %v", err)
	}
	if _, err = testServer(t).importZoteroEntry(context.Background(), linked.Entries[0]); err != nil {
		t.Fatal(err)
	}
}
func TestZoteroTransactionRollbackAndDuplicateProtection(t *testing.T) {
	dir, _ := zoteroFixture(t)
	entry := mustScanZotero(t, dir).Entries[0]
	s := testServer(t)
	if _, err := s.Store.DB.Exec(`CREATE TRIGGER reject_zotero BEFORE INSERT ON zotero_imports BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.importZoteroEntry(context.Background(), entry); err == nil {
		t.Fatal("expected failure")
	}
	docs, err := s.Store.Documents()
	if err != nil || len(docs) != 0 {
		t.Fatal("partially imported document", err)
	}
	files, err := os.ReadDir(filepath.Join(s.Store.Root, "papers"))
	if err != nil || len(files) != 0 {
		t.Fatal("orphaned file", err)
	}
	if _, err = s.Store.DB.Exec(`DROP TRIGGER reject_zotero`); err != nil {
		t.Fatal(err)
	}
	response := uploadTo(t, s, "papers", "original.pdf", testPDF(2))
	if response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	var doc Document
	json.Unmarshal(response.Body.Bytes(), &doc)
	if response = request(t, s, "PATCH", "/api/documents/"+doc.ID, strings.NewReader(`{"title":"NeoRead 标题","metadata":{"doi":"10.9999/edited"},"tags":["existing"]}`)); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	result, err := s.importZoteroEntry(context.Background(), entry)
	if err != nil || result.Status != "merged" {
		t.Fatal(result, err)
	}
	doc, err = s.Store.Document(doc.ID)
	if err != nil || doc.Title != "NeoRead 标题" || doc.Metadata.DOI != "10.9999/edited" || len(doc.Tags) != 2 {
		t.Fatal(doc, err)
	}
}
func TestZoteroPathsAndPreviewLifecycle(t *testing.T) {
	for _, path := range []string{"storage:../bad.pdf", "storage:/bad.pdf", "storage:..\\bad.pdf", "other.pdf"} {
		if _, _, err := zoteroAttachmentPath("/tmp", "", "PDF00001", path, 0); err == nil {
			t.Fatal(path)
		}
	}
	if _, _, err := zoteroAttachmentPath("/tmp", "/tmp/base", "PDF00001", "attachments:../bad.pdf", 2); err == nil {
		t.Fatal("relative traversal")
	}
	dir, _ := zoteroFixture(t)
	entry := mustScanZotero(t, dir).Entries[0]
	outside := filepath.Join(t.TempDir(), "outside.pdf")
	os.WriteFile(outside, testPDF(1), 0600)
	os.Remove(entry.path)
	if err := os.Symlink(outside, entry.path); err != nil {
		t.Fatal(err)
	}
	s := testServer(t)
	if _, err := s.importZoteroEntry(context.Background(), entry); err == nil {
		t.Fatal("symlink escape")
	}
	if w := request(t, s, "POST", "/api/import/zotero", strings.NewReader(`{"scanId":"missing","entryId":"anything"}`)); w.Code != 409 {
		t.Fatal(w.Body.String())
	}
	scan := mustScanZotero(t, dir)
	scan.expires = time.Now().Add(-time.Second)
	s.zoteroScans = map[string]*zoteroScan{scan.ID: scan}
	body, _ := json.Marshal(map[string]string{"scanId": scan.ID, "entryId": entry.ID})
	if w := request(t, s, "POST", "/api/import/zotero", bytes.NewReader(body)); w.Code != 409 {
		t.Fatal("expired preview accepted")
	}
}
func TestZoteroRotatedCoordinatesAndFallback(t *testing.T) {
	box := [4]float64{10, 20, 110, 220}
	rect := []float64{20, 40, 50, 80}
	cases := []struct {
		rotation   int
		x, y, w, h float64
	}{{0, .1, .7, .3, .2}, {90, .1, .1, .2, .3}, {180, .6, .1, .3, .2}, {270, .7, .6, .2, .3}}
	for _, tt := range cases {
		got, ok := zoteroRect(rect, box, tt.rotation)
		if !ok || math.Abs(got.X-tt.x) > 1e-8 || math.Abs(got.Y-tt.y) > 1e-8 || math.Abs(got.Width-tt.w) > 1e-8 || math.Abs(got.Height-tt.h) > 1e-8 {
			t.Fatal(tt, got)
		}
	}
	dir, _ := zoteroFixture(t)
	entry := mustScanZotero(t, dir).Entries[0]
	for _, position := range []string{`{}`, `{"pageIndex":-1}`, `{"pageIndex":100,"rects":[[1,2,3,4]]}`, `{"pageIndex":0,"rects":[[1,2,3]]}`, `{"pageIndex":0,"rects":[[1,2,3,4]],"nextPageRects":[[1,2,3,4]]}`} {
		a, w := zoteroConvertAnnotation(entry.path, "doc", 1, zoteroAnnotation{Key: "BAD", Type: 1, Position: position})
		if a != nil || w == "" {
			t.Fatal(position)
		}
	}
}

func TestZoteroChangedFileAndMetadataBoundaries(t *testing.T) {
	dir, _ := zoteroFixture(t)
	entry := mustScanZotero(t, dir).Entries[0]
	if err := os.WriteFile(entry.path, testPDF(3), 0600); err != nil {
		t.Fatal(err)
	}
	s := testServer(t)
	if _, err := s.importZoteroEntry(context.Background(), entry); err == nil || !strings.Contains(err.Error(), "扫描后发生变化") {
		t.Fatalf("changed file: %v", err)
	}
	cases := map[string]string{"2024-00-00 2024": "2024", "2024-06-00 June 2024": "2024-06", "2024-06-12 June 12": "2024-06-12", "2024": "2024", "Spring 2024": "Spring 2024"}
	for input, want := range cases {
		if got := zoteroMetadataDate(input); got != want {
			t.Errorf("%q: %q != %q", input, got, want)
		}
	}
	labels, warnings := zoteroLabels([]string{"Existing"}, []string{"existing", strings.Repeat("长", 41), "Valid"}, "分类")
	if len(labels) != 2 || len(warnings) != 1 {
		t.Fatal(labels, warnings)
	}
}

func TestZoteroHTTPScanAndImport(t *testing.T) {
	dir, _ := zoteroFixture(t)
	s := testServer(t)
	body, _ := json.Marshal(map[string]string{"directory": dir})
	response := request(t, s, "POST", "/api/import/zotero/scan", bytes.NewReader(body))
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var scan zoteroScan
	if err := json.Unmarshal(response.Body.Bytes(), &scan); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(response.Body.String(), "storage:") || strings.Contains(response.Body.String(), "我的笔记") {
		t.Fatal("private snapshot data exposed in preview")
	}
	body, _ = json.Marshal(map[string]string{"scanId": scan.ID, "entryId": scan.Entries[0].ID})
	response = request(t, s, "POST", "/api/import/zotero", bytes.NewReader(body))
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var result zoteroImportResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Status != "imported" {
		t.Fatal(response.Body.String(), err)
	}
}

func TestZoteroLockedLibraryCanRetry(t *testing.T) {
	for _, mode := range []string{"DELETE", "WAL"} {
		t.Run(mode, func(t *testing.T) {
			dir, owner := zoteroFixture(t)
			owner.SetMaxOpenConns(1)
			// Zotero keeps an exclusive connection-lifetime lock even between writes.
			if _, err := owner.Exec(`PRAGMA journal_mode=` + mode + `; PRAGMA locking_mode=EXCLUSIVE; BEGIN EXCLUSIVE; COMMIT`); err != nil {
				t.Fatal(err)
			}
			s := testServer(t)
			body, err := json.Marshal(map[string]string{"directory": dir})
			if err != nil {
				t.Fatal(err)
			}
			response := request(t, s, "POST", "/api/import/zotero/scan", bytes.NewReader(body))
			var failure struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
				t.Fatal(err)
			}
			if response.Code != 409 || failure.Code != "zotero-locked" || !strings.Contains(failure.Error, "完全退出 Zotero") || strings.Contains(failure.Error, "SQLITE_BUSY") {
				t.Errorf("locked library needs actionable guidance: status=%d body=%s", response.Code, response.Body.String())
			}
			if len(s.zoteroScans) != 0 {
				t.Fatal("failed scan cached a partial preview")
			}
			docs, err := s.Store.Documents()
			if err != nil || len(docs) != 0 {
				t.Fatalf("failed scan changed NeoRead: %v %v", docs, err)
			}
			// Closing the owner's connection models quitting Zotero, without changing
			// lock settings or copying an active database. The same scan can then retry.
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			response = request(t, s, "POST", "/api/import/zotero/scan", bytes.NewReader(body))
			if response.Code != 200 {
				t.Fatalf("scan after release: %d %s", response.Code, response.Body.String())
			}
			var scan zoteroScan
			if err := json.Unmarshal(response.Body.Bytes(), &scan); err != nil || len(scan.Entries) != 1 {
				t.Fatalf("incomplete preview after release: %+v %v", scan, err)
			}
		})
	}
}

func TestZoteroStripBraces(t *testing.T) {
	cases := map[string]string{
		"{NAUTILUS}: Automated {RESTful} {API} Vulnerability Detection": "NAUTILUS: Automated RESTful API Vulnerability Detection",
		"{{A}B} and {C{D}}":         "AB and CD",
		`Sets \{x\} in {TLS}`:       `Sets \{x\} in {TLS}`,
		`On \textit{Fuzzing} {TLS}`: `On \textit{Fuzzing} {TLS}`,
		"Unbalanced {TLS":           "Unbalanced {TLS",
		"Unbalanced} {TLS}":         "Unbalanced} {TLS}",
		"}{":                        "}{",
		"Plain Title: No Braces":    "Plain Title: No Braces",
		"":                          "",
	}
	for input, want := range cases {
		if got := zoteroStripBraces(input); got != want {
			t.Errorf("%q: %q != %q", input, got, want)
		}
	}
}

func TestZoteroImportStripsTitleBraces(t *testing.T) {
	braced := func(t *testing.T) *zoteroEntry {
		t.Helper()
		dir, db := zoteroFixture(t)
		if _, err := db.Exec(`UPDATE itemDataValues SET value='{PolyFuzz}: Holistic Greybox Fuzzing of {Multi-Language} Systems' WHERE valueID=1; INSERT INTO fields VALUES(5,'shortTitle'); INSERT INTO itemDataValues VALUES(5,'{PolyFuzz}'); INSERT INTO itemData VALUES(1,5,5)`); err != nil {
			t.Fatal(err)
		}
		return mustScanZotero(t, dir).Entries[0]
	}
	t.Run("fresh", func(t *testing.T) {
		s := testServer(t)
		result, err := s.importZoteroEntry(context.Background(), braced(t))
		if err != nil || result.Status != "imported" {
			t.Fatal(result, err)
		}
		doc, err := s.Store.Document(result.DocumentID)
		if err != nil {
			t.Fatal(err)
		}
		if doc.Title != "PolyFuzz: Holistic Greybox Fuzzing of Multi-Language Systems" || doc.Metadata.ShortTitle != "PolyFuzz" || doc.Metadata.Sources["shortTitle"] != "manual" {
			t.Fatalf("document: %q %q %+v", doc.Title, doc.Metadata.ShortTitle, doc.Metadata.Sources)
		}
		var snapshot string
		if err = s.Store.DB.QueryRow(`SELECT body FROM zotero_imports WHERE document_id=?`, doc.ID).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(snapshot, "{PolyFuzz}") || !strings.Contains(snapshot, "{Multi-Language}") {
			t.Fatal("migration snapshot must keep original values", snapshot)
		}
	})
	for name, patch := range map[string]string{
		"manual values": `{"title":"NeoRead 标题","metadata":{"shortTitle":"我的短标题"}}`,
		"manual empty":  `{"title":"NeoRead 标题","metadata":{"shortTitle":""}}`,
	} {
		t.Run(name, func(t *testing.T) {
			entry := braced(t)
			s := testServer(t)
			response := uploadTo(t, s, "papers", "original.pdf", testPDF(2))
			if response.Code != 201 {
				t.Fatal(response.Body.String())
			}
			var doc Document
			json.Unmarshal(response.Body.Bytes(), &doc)
			if response = request(t, s, "PATCH", "/api/documents/"+doc.ID, strings.NewReader(patch)); response.Code != 200 {
				t.Fatal(response.Body.String())
			}
			var want Document
			json.Unmarshal(response.Body.Bytes(), &want)
			result, err := s.importZoteroEntry(context.Background(), entry)
			if err != nil || result.Status != "merged" {
				t.Fatal(result, err)
			}
			doc, err = s.Store.Document(doc.ID)
			if err != nil || doc.Title != "NeoRead 标题" || doc.Metadata.ShortTitle != want.Metadata.ShortTitle {
				t.Fatalf("manual values overwritten: %q %q %v", doc.Title, doc.Metadata.ShortTitle, err)
			}
		})
	}
}

func TestZoteroDuplicateKeepsLongestNote(t *testing.T) {
	dir, _ := zoteroFixture(t)
	entry := mustScanZotero(t, dir).Entries[0]
	baseline := testServer(t)
	result, err := baseline.importZoteroEntry(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	var incoming string
	if err := baseline.Store.DB.QueryRow("SELECT body FROM document_notes WHERE document_id=?", result.DocumentID).Scan(&incoming); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, existing, want string }{
		{"longer import", "短", incoming},
		{"longer existing", strings.Repeat("现", len([]rune(incoming))+1), strings.Repeat("现", len([]rune(incoming))+1)},
		{"equal keeps existing", strings.Repeat("😀", len([]rune(incoming))), strings.Repeat("😀", len([]rune(incoming)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t)
			response := uploadTo(t, s, "papers", "original.pdf", testPDF(2))
			if response.Code != 201 {
				t.Fatal(response.Body.String())
			}
			var doc Document
			if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Store.DB.Exec("INSERT INTO document_notes(document_id,body,updated_at) VALUES(?,?,?)", doc.ID, tc.existing, now()); err != nil {
				t.Fatal(err)
			}
			if _, err := s.importZoteroEntry(context.Background(), entry); err != nil {
				t.Fatal(err)
			}
			var got string
			if err := s.Store.DB.QueryRow("SELECT body FROM document_notes WHERE document_id=?", doc.ID).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("note = %q, want %q", got, tc.want)
			}
		})
	}
}

func zoteroProfileFixture(t *testing.T, root, name, directory, base string) string {
	t.Helper()
	profile := filepath.Join(root, name)
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	dataDir, _ := json.Marshal(directory)
	linkedBase, _ := json.Marshal(base)
	prefs := "user_pref(\"extensions.zotero.useDataDir\", true);\n" +
		"user_pref(\"extensions.zotero.dataDir\", " + string(dataDir) + ");\n" +
		"user_pref(\"extensions.zotero.baseAttachmentPath\", " + string(linkedBase) + ");\n"
	if err := os.WriteFile(filepath.Join(profile, "prefs.js"), []byte(prefs), 0600); err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestZoteroPathPreferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.js")
	text := `// Other preferences and JavaScript are ignored, never evaluated.
user_pref("unrelated", "ignored");
user_pref("extensions.zotero.useDataDir", true);
user_pref("extensions.zotero.dataDir", "C:\\Zotero");
user_pref("extensions.zotero.baseAttachmentPath", "/old");
  user_pref( "extensions.zotero.baseAttachmentPath" , "/论文/\"quoted\"/\u4e2d文" );
run_code();
`
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	prefs, err := readZoteroPathPreferences(path)
	if err != nil || !prefs.useDataDir || prefs.dataDir != `C:\Zotero` || prefs.linkedBase != "/论文/\"quoted\"/中文" {
		t.Fatalf("preferences: %+v, %v", prefs, err)
	}
	for _, invalid := range []string{
		`user_pref("extensions.zotero.baseAttachmentPath", getPath());`,
		`user_pref("extensions.zotero.useDataDir", "true");`,
		`user_pref("extensions.zotero.baseAttachmentPath", "/injected"); run_code();`,
	} {
		if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readZoteroPathPreferences(path); err == nil {
			t.Fatalf("accepted invalid preference: %s", invalid)
		}
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), (4<<20)+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readZoteroPathPreferences(path); err == nil {
		t.Fatal("accepted oversized preferences")
	}
}

func TestZoteroProfileDiscovery(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	ini := "[Profile0]\nIsRelative=1\nPath=Profiles/relative.default\n" +
		"[Profile1]\nPath=" + outside + "\nIsRelative=0\n" +
		"[General]\nPath=Profiles/not-a-profile\n" +
		"[Profile2]\nIsRelative=1\nPath=../escape\n"
	if err := os.WriteFile(filepath.Join(root, "profiles.ini"), []byte(ini), 0600); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "Profiles", "relative.default"), outside}
	if got := zoteroProfileDirectories(root); !reflect.DeepEqual(got, want) {
		t.Fatalf("registered profiles: %v", got)
	}
	if err := os.Remove(filepath.Join(root, "profiles.ini")); err != nil {
		t.Fatal(err)
	}
	mac := zoteroProfileFixture(t, root, "Profiles/mac.default", t.TempDir(), t.TempDir())
	linux := zoteroProfileFixture(t, root, "linux.default", t.TempDir(), t.TempDir())
	if got := zoteroProfileDirectories(root); !reflect.DeepEqual(got, []string{mac, linux}) {
		t.Fatalf("standard layouts: %v", got)
	}
	for _, tc := range []struct{ platform, appData, want string }{
		{"darwin", "", filepath.Join(root, "Library", "Application Support", "Zotero")},
		{"linux", "", filepath.Join(root, ".zotero", "zotero")},
		{"windows", outside, filepath.Join(outside, "Zotero", "Zotero")},
		{"windows", "", filepath.Join(root, "AppData", "Roaming", "Zotero", "Zotero")},
	} {
		if got := zoteroProfileRoot(tc.platform, root, tc.appData); got != tc.want {
			t.Fatalf("%s profile root: %s", tc.platform, got)
		}
	}
}

func TestZoteroLinkedBaseMatchesLibrary(t *testing.T) {
	root, directory, base := t.TempDir(), t.TempDir(), t.TempDir()
	other := zoteroProfileFixture(t, root, "other", t.TempDir(), t.TempDir())
	matching := zoteroProfileFixture(t, root, "matching", directory, base)
	resolvedBase, _ := filepath.EvalSymlinks(base)
	got, err := zoteroLinkedBaseFromProfiles(directory, t.TempDir(), []string{other, matching})
	if err != nil || got != resolvedBase {
		t.Fatalf("matching library: %s, %v", got, err)
	}
	// A copied database must not borrow the original library's configuration.
	if _, err := zoteroLinkedBaseFromProfiles(t.TempDir(), directory, []string{matching}); err == nil {
		t.Fatal("unrelated library accepted")
	}
	alias := filepath.Join(root, "library-alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	if got, err := zoteroLinkedBaseFromProfiles(alias, t.TempDir(), []string{matching}); err != nil || got != resolvedBase {
		t.Fatalf("symlinked library: %s, %v", got, err)
	}
	duplicate := zoteroProfileFixture(t, root, "duplicate", directory, base)
	if got, err := zoteroLinkedBaseFromProfiles(directory, t.TempDir(), []string{matching, duplicate}); err != nil || got != resolvedBase {
		t.Fatalf("equivalent profiles: %s, %v", got, err)
	}
	conflicting := zoteroProfileFixture(t, root, "conflicting", directory, t.TempDir())
	if _, err := zoteroLinkedBaseFromProfiles(directory, t.TempDir(), []string{matching, conflicting}); err == nil || !strings.Contains(err.Error(), "多个") {
		t.Fatalf("conflicting profiles: %v", err)
	}
	for _, base := range []string{"", "relative", filepath.Join(root, "missing")} {
		profile := zoteroProfileFixture(t, root, "invalid", directory, base)
		if _, err := zoteroLinkedBaseFromProfiles(directory, t.TempDir(), []string{profile}); err == nil {
			t.Fatalf("accepted invalid base: %q", base)
		}
	}
	// Without a custom data directory preference, match the standard data directory.
	if err := os.WriteFile(filepath.Join(matching, "prefs.js"), []byte("user_pref(\"extensions.zotero.baseAttachmentPath\", "+string(mustJSON(base))+");\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := zoteroLinkedBaseFromProfiles(directory, directory, []string{matching}); err != nil || got != resolvedBase {
		t.Fatalf("default library: %s, %v", got, err)
	}
}

func TestZoteroAutoLinkedBaseScanAndImport(t *testing.T) {
	directory, db := zoteroFixture(t)
	base, root := t.TempDir(), t.TempDir()
	profile := zoteroProfileFixture(t, root, "Profiles/linked.default", directory, base)
	if err := os.WriteFile(filepath.Join(root, "profiles.ini"), []byte("[Profile0]\nIsRelative=1\nPath=Profiles/linked.default\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "论文"), 0700); err != nil {
		t.Fatal(err)
	}
	pdf := filepath.Join(base, "论文", "linked.pdf")
	if err := os.WriteFile(pdf, testPDF(1), 0600); err != nil {
		t.Fatal(err)
	}
	resolvedPDF, err := filepath.EvalSymlinks(pdf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE itemAttachments SET path='attachments:论文/linked.pdf',linkMode=2 WHERE itemID=2;
INSERT INTO items VALUES(7,2,1,'LINKED02','2024');
INSERT INTO itemAttachments VALUES(7,1,'attachments:论文\linked.pdf',2,'application/pdf');`); err != nil {
		t.Fatal(err)
	}
	prefsBefore, _ := os.ReadFile(filepath.Join(profile, "prefs.js"))
	dbBefore, _ := os.ReadFile(filepath.Join(directory, "zotero.sqlite"))
	calls := 0
	resolve := func(selected string) (string, error) {
		calls++
		return zoteroLinkedBaseFromProfiles(selected, t.TempDir(), zoteroProfileDirectories(root))
	}
	scan, err := readZoteroWithBaseResolver(context.Background(), directory, "", resolve)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(scan.Entries) != 2 || len(scan.Warnings) != 1 || !strings.Contains(scan.Warnings[0], base) {
		t.Fatalf("auto discovery: calls=%d, scan=%+v", calls, scan)
	}
	for _, entry := range scan.Entries {
		if entry.Issue != "" || entry.path != resolvedPDF {
			t.Fatalf("linked entry: %+v", entry)
		}
	}
	s := testServer(t)
	result, err := s.importZoteroEntry(context.Background(), scan.Entries[0])
	if err != nil || result.Status != "imported" {
		t.Fatalf("import: %+v, %v", result, err)
	}
	prefsAfter, _ := os.ReadFile(filepath.Join(profile, "prefs.js"))
	dbAfter, _ := os.ReadFile(filepath.Join(directory, "zotero.sqlite"))
	if !bytes.Equal(prefsBefore, prefsAfter) || !bytes.Equal(dbBefore, dbAfter) {
		t.Fatal("source configuration or database changed")
	}
	// Manual input takes precedence, even when automatic discovery cannot resolve a base.
	scan, err = readZoteroWithBaseResolver(context.Background(), directory, base, func(string) (string, error) {
		t.Fatal("manual override invoked discovery")
		return "", nil
	})
	if err != nil || scan.Entries[0].Issue != "" || len(scan.Warnings) != 0 {
		t.Fatalf("manual override: %+v, %v", scan, err)
	}
	for _, path := range []string{`attachments:..\escape.pdf`, `attachments:论文\..\..\escape.pdf`} {
		if _, _, err := zoteroAttachmentPath(directory, base, "PDF00001", path, 2); err == nil {
			t.Fatalf("accepted traversal: %s", path)
		}
	}
}

func TestZoteroAutoLinkedBaseFailureKeepsOtherAttachments(t *testing.T) {
	directory, db := zoteroFixture(t)
	if _, err := db.Exec(`INSERT INTO items VALUES(7,2,1,'LINKED02','2024'),(8,2,1,'LINKED03','2024');
INSERT INTO itemAttachments VALUES(7,1,'attachments:missing.pdf',2,'application/pdf'),(8,1,'attachments:also-missing.pdf',2,'application/pdf');`); err != nil {
		t.Fatal(err)
	}
	calls := 0
	scan, err := readZoteroWithBaseResolver(context.Background(), directory, "", func(selected string) (string, error) {
		calls++
		return zoteroLinkedBaseFromProfiles(selected, t.TempDir(), nil)
	})
	if err != nil || calls != 1 || len(scan.Entries) != 3 {
		t.Fatalf("scan: %+v, calls=%d, err=%v", scan, calls, err)
	}
	if scan.Entries[0].Issue != "" || !strings.Contains(scan.Entries[1].Issue, "手动填写") || scan.Entries[1].Issue != scan.Entries[2].Issue {
		t.Fatalf("attachment issues: %+v", scan.Entries)
	}
	// Neither stored PDFs nor absolute linked PDFs need profile discovery.
	absolute := filepath.Join(t.TempDir(), "absolute.pdf")
	if err := os.WriteFile(absolute, testPDF(1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE itemAttachments SET path=? WHERE itemID IN (7,8)`, absolute); err != nil {
		t.Fatal(err)
	}
	scan, err = readZoteroWithBaseResolver(context.Background(), directory, "", func(string) (string, error) {
		t.Fatal("stored or absolute attachment invoked discovery")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range scan.Entries {
		if entry.Issue != "" {
			t.Fatalf("ordinary attachment: %+v", entry)
		}
	}
}

func endnoteFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "Test.enl")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE refs(id INTEGER,reference_type INTEGER,title TEXT,author TEXT,year TEXT,secondary_title TEXT,volume TEXT,number TEXT,pages TEXT,publisher TEXT,electronic_resource_number TEXT,url TEXT,abstract TEXT,keywords TEXT,notes TEXT,research_notes TEXT,trash_state INTEGER); CREATE TABLE file_res(refs_id INTEGER,file_path TEXT,file_pos INTEGER);INSERT INTO refs VALUES(1,0,'论文','Ada Lovelace','2025','Journal','1','2','3-4','Publisher','10.1000/test','','Abstract','tag','note','research',0); INSERT INTO file_res VALUES(1,'123/paper.pdf',0);`)
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(dir, "Test.Data", "PDF", "123")
	if err = os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(folder, "paper.pdf"), testPDF(2), 0600); err != nil {
		t.Fatal(err)
	}
	return path, db
}
func TestEndnoteReadOnlyAndPaths(t *testing.T) {
	path, db := endnoteFixture(t)
	before, _ := os.ReadFile(path)
	scan, err := endnoteSource(context.Background(), filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Entries) != 1 || scan.Entries[0].Issue != "" || scan.Entries[0].metadata.DOI != "10.1000/test" {
		t.Fatalf("%+v", scan)
	}
	after, _ := os.ReadFile(path)
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("source database changed")
	}
	if _, err = endnoteLibraryPath(strings.TrimSuffix(path, ".enl") + ".Data"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE file_res SET file_path='../../escape.pdf'"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(filepath.Dir(path), "Test.Data", "escape.pdf"), testPDF(1), 0600); err != nil {
		t.Fatal(err)
	}
	scan, err = endnoteSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if scan.Entries[0].Issue == "" {
		t.Fatal("escape accepted")
	}
}
func TestEndnoteLockedLibraryAndAmbiguousDirectory(t *testing.T) {
	path, db := endnoteFixture(t)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	if _, err = endnoteSource(context.Background(), path); err == nil {
		t.Fatal("locked source accepted")
	}
	conn.ExecContext(context.Background(), "ROLLBACK")
	os.WriteFile(filepath.Join(filepath.Dir(path), "Other.enl"), nil, 0600)
	if _, err = endnoteLibraryPath(filepath.Dir(path)); err == nil {
		t.Fatal("ambiguous source accepted")
	}
}
func TestEndnoteImportAtomicAndRepeatable(t *testing.T) {
	path, _ := endnoteFixture(t)
	scan, err := endnoteSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	e := scan.Entries[0]
	original, _ := os.ReadFile(e.path)
	s := testServer(t)
	calls := 0
	convert := func(ctx context.Context, input, work string) (string, endnotePDFResult, error) {
		calls++
		dest := filepath.Join(work, "cleaned.pdf")
		if err := os.WriteFile(dest, testPDF(1), 0600); err != nil {
			return "", endnotePDFResult{}, err
		}
		return dest, endnotePDFResult{Marks: []endnotePDFMark{{Key: "1:4R", Kind: "underline", Location: json.RawMessage(`{"type":"pdf","page":1,"rects":[{"x":0.1,"y":0.1,"width":0.2,"height":0.03}]}`), Color: "#fc6a08"}}}, nil
	}
	result, err := s.importEndnoteEntry(context.Background(), e, "papers", true, convert)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "imported" || result.Annotations != 1 || result.Notes != 1 {
		t.Fatalf("%+v", result)
	}
	if _, err = s.Store.DB.Exec("DELETE FROM annotations WHERE document_id=?", result.DocumentID); err != nil {
		t.Fatal(err)
	}
	again, err := s.importEndnoteEntry(context.Background(), e, "papers", true, convert)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != "exists" || calls != 1 {
		t.Fatalf("%+v calls=%d", again, calls)
	}
	var count int
	s.Store.DB.QueryRow("SELECT count(*) FROM annotations").Scan(&count)
	if count != 0 {
		t.Fatal("deleted annotation resurrected")
	}
	after, _ := os.ReadFile(e.path)
	if sha256.Sum256(original) != sha256.Sum256(after) {
		t.Fatal("source PDF changed")
	}
	entries, _ := os.ReadDir(s.Store.Root)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "endnote-") {
			t.Fatal("temporary original retained")
		}
	}
}
func TestEndnoteFailedConversionLeavesNoDocument(t *testing.T) {
	path, _ := endnoteFixture(t)
	scan, err := endnoteSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	s := testServer(t)
	_, err = s.importEndnoteEntry(context.Background(), scan.Entries[0], "papers", true, func(context.Context, string, string) (string, endnotePDFResult, error) {
		return "", endnotePDFResult{}, errors.New("failure")
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	var count int
	s.Store.DB.QueryRow("SELECT count(*) FROM documents").Scan(&count)
	if count != 0 {
		t.Fatal("partial document committed")
	}
}

// Opt-in integration check against a disposable SQLite snapshot and local PDFs.
// Never requires a private library in CI and never edits the source library.
func TestEndnoteLiveSnapshot(t *testing.T) {
	path := os.Getenv("NEOREAD_TEST_ENDNOTE_LIBRARY")
	if path == "" {
		t.Skip("set NEOREAD_TEST_ENDNOTE_LIBRARY to a disposable snapshot")
	}
	scan, err := endnoteSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	s := testServer(t)
	total := 0
	for _, e := range scan.Entries {
		if e.Issue != "" {
			t.Fatalf("unavailable sample: %s", e.Issue)
		}
		before, err := os.ReadFile(e.path)
		if err != nil {
			t.Fatal(err)
		}
		result, err := s.importEndnoteEntry(context.Background(), e, "papers", true, migrateEndnotePDF)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "imported" {
			t.Fatalf("%+v", result)
		}
		total += result.Annotations
		again, err := s.importEndnoteEntry(context.Background(), e, "papers", true, migrateEndnotePDF)
		if err != nil || again.Status != "exists" {
			t.Fatalf("retry: %+v %v", again, err)
		}
		after, _ := os.ReadFile(e.path)
		if sha256.Sum256(before) != sha256.Sum256(after) {
			t.Fatal("source changed")
		}
	}
	if total == 0 {
		t.Fatal("sample has no converted marks")
	}
	t.Logf("imported %d PDFs and %d native annotations; repeat skipped; sources unchanged", len(scan.Entries), total)
}

func TestEndnoteMixedAttachmentLimitIsExplicit(t *testing.T) {
	path, db := endnoteFixture(t)
	_, err := db.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<20001) INSERT INTO file_res SELECT 1,'other.txt',x FROM n`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = endnoteSource(context.Background(), path); err == nil || !strings.Contains(err.Error(), "20000") {
		t.Fatalf("expected explicit limit, got %v", err)
	}
}

func TestEndnoteNullableMetadataDoesNotBlockOtherPapers(t *testing.T) {
	path, db := endnoteFixture(t)
	_, err := db.Exec(`INSERT INTO refs (id,reference_type,trash_state) VALUES(2,0,0); INSERT INTO file_res VALUES(2,'123/nullable.pdf',0)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "Test.Data", "PDF", "123", "nullable.pdf"), testPDF(1), 0600); err != nil {
		t.Fatal(err)
	}
	scan, err := endnoteSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Entries) != 2 {
		t.Fatalf("expected two papers, got %d", len(scan.Entries))
	}
	regular, nullable := scan.Entries[0], scan.Entries[1]
	if regular.metadata.DOI != "10.1000/test" || regular.Title != "论文" || regular.Notes != 1 {
		t.Fatalf("populated metadata lost: %+v", regular)
	}
	if nullable.Title != "nullable" || nullable.Notes != 0 || nullable.note != "" || len(nullable.Tags) != 0 || len(nullable.metadata.Creators) != 0 {
		t.Fatalf("unexpected NULL defaults: %+v", nullable)
	}
	if nullable.metadata.Date != "" || nullable.metadata.Venue != "" || nullable.metadata.Volume != "" || nullable.metadata.Issue != "" || nullable.metadata.Pages != "" || nullable.metadata.Publisher != "" || nullable.metadata.DOI != "" || nullable.metadata.URL != "" || nullable.metadata.Abstract != "" {
		t.Fatalf("unexpected metadata: %+v", nullable.metadata)
	}
	s := testServer(t)
	convert := func(ctx context.Context, input, work string) (string, endnotePDFResult, error) {
		data, err := os.ReadFile(input)
		if err != nil {
			return "", endnotePDFResult{}, err
		}
		dest := filepath.Join(work, "cleaned.pdf")
		return dest, endnotePDFResult{}, os.WriteFile(dest, data, 0600)
	}
	for _, e := range scan.Entries {
		if e.Issue != "" {
			t.Fatalf("unavailable PDF: %s", e.Issue)
		}
		result, err := s.importEndnoteEntry(context.Background(), e, "papers", true, convert)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "imported" {
			t.Fatalf("import failed: %+v", result)
		}
	}
	var count int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM documents").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected two imported documents, got %d", count)
	}
}

func TestEndnoteDifferentSourceSamePDFReportsUnmergedData(t *testing.T) {
	path, db := endnoteFixture(t)
	_, err := db.Exec(`INSERT INTO refs (id,reference_type,title,notes,keywords,trash_state) VALUES(2,0,'Other title','Other note','Other tag',0); INSERT INTO file_res VALUES(2,'123/paper.pdf',0)`)
	if err != nil {
		t.Fatal(err)
	}
	scan, err := endnoteSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	s := testServer(t)
	calls := 0
	convert := func(ctx context.Context, input, work string) (string, endnotePDFResult, error) {
		calls++
		return input, endnotePDFResult{}, nil
	}
	first, err := s.importEndnoteEntry(context.Background(), scan.Entries[0], "papers", true, convert)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		result, err := s.importEndnoteEntry(context.Background(), scan.Entries[1], "papers", true, convert)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "exists" || !strings.Contains(strings.Join(result.Warnings, ""), "未合并此来源的标题、元数据、标签和笔记") {
			t.Fatalf("silent skip: %+v", result)
		}
	}
	repeat, err := s.importEndnoteEntry(context.Background(), scan.Entries[0], "papers", true, convert)
	if err != nil || repeat.Status != "exists" || len(repeat.Warnings) != 0 || calls != 1 {
		t.Fatalf("repeat: %+v %v calls=%d", repeat, err, calls)
	}
	doc, err := s.Store.Document(first.DocumentID)
	if err != nil || doc.Title != "论文" {
		t.Fatalf("existing title changed: %+v %v", doc, err)
	}
	var note string
	if err := s.Store.DB.QueryRow("SELECT body FROM document_notes WHERE document_id=?", first.DocumentID).Scan(&note); err != nil || note != "note\n\nresearch" {
		t.Fatalf("existing note changed: %q %v", note, err)
	}
	var count int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM endnote_imports").Scan(&count); err != nil || count != 1 {
		t.Fatalf("unexpected migration records: %d %v", count, err)
	}
}

func TestEndnoteRejectedMetadataIsReportedAndArchived(t *testing.T) {
	path, db := endnoteFixture(t)
	abstract := strings.Repeat("a", 20001)
	if _, err := db.Exec("UPDATE refs SET year=?,url=?,abstract=?,author=?", "2024a", "ftp://example.org/paper", abstract, strings.Repeat("b", 201)); err != nil {
		t.Fatal(err)
	}
	scan, err := endnoteSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	s := testServer(t)
	result, err := s.importEndnoteEntry(context.Background(), scan.Entries[0], "papers", true, func(ctx context.Context, input, work string) (string, endnotePDFResult, error) {
		return input, endnotePDFResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "imported" {
		t.Fatalf("%+v", result)
	}
	warnings := strings.Join(result.Warnings, "\n")
	for _, key := range []string{"date", "url", "abstract", "creators"} {
		if !strings.Contains(warnings, metadataLabels[key]+"未转换") {
			t.Fatalf("missing %s warning: %s", key, warnings)
		}
	}
	doc, err := s.Store.Document(result.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Metadata.Date != "" || doc.Metadata.URL != "" || doc.Metadata.Abstract != "" || len(doc.Metadata.Creators) != 0 || doc.Author != "" || doc.Metadata.DOI != "10.1000/test" {
		t.Fatalf("unexpected metadata: %+v", doc)
	}
	var raw []byte
	if err := s.Store.DB.QueryRow("SELECT body FROM endnote_imports WHERE source_id=?", scan.Entries[0].ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var archive struct {
		Metadata PaperMetadata `json:"metadata"`
		Warnings []string      `json:"warnings"`
	}
	if err := json.Unmarshal(raw, &archive); err != nil {
		t.Fatal(err)
	}
	if archive.Metadata.Date != "2024a" || archive.Metadata.URL != "ftp://example.org/paper" || archive.Metadata.Abstract != abstract || len(archive.Metadata.Creators[0].Name) != 201 || len(archive.Warnings) != len(result.Warnings) {
		t.Fatal("source values or warnings missing from archive")
	}
}

func TestNeoReadCitationCredentialCompatibility(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "Reader")
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("a", 64)
	if err := os.WriteFile(filepath.Join(legacy, "cite-wps-key"), []byte(key), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	target := filepath.Join(root, "NeoRead", "cite-credential")
	if err := s.SetCiteCredential(target); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(filepath.Join(legacy, "cite-credential"))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(old) {
		t.Fatal("legacy plugin credential differs")
	}
	migrated, err := os.ReadFile(filepath.Join(root, "NeoRead", "cite-wps-key"))
	if err != nil {
		t.Fatal(err)
	}
	if string(migrated) != key {
		t.Fatal("WPS installation key changed")
	}
}

type citeHarness struct {
	t         *testing.T
	server    *Server
	api       *httptest.Server
	connector *httptest.Server
	secret    string
}

func newCiteHarness(t *testing.T) *citeHarness {
	t.Helper()
	s := testServer(t)
	path := filepath.Join(t.TempDir(), "Reader", "cite-credential")
	if err := s.SetCiteCredential(path); err != nil {
		t.Fatal(err)
	}
	secret, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &citeHarness{t: t, server: s, api: httptest.NewServer(s.Handler()), connector: httptest.NewServer(s.ConnectorHandler()), secret: string(secret)}
	t.Cleanup(h.api.Close)
	t.Cleanup(h.connector.Close)
	return h
}

func (h *citeHarness) client(path, body, secret string) (int, string) {
	h.t.Helper()
	r, _ := http.NewRequest("POST", h.connector.URL+path, strings.NewReader(body))
	r.Host = "127.0.0.1:" + ConnectorPort
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+secret)
	return h.do(r)
}

func (h *citeHarness) reader(method, path, body string) (int, string) {
	h.t.Helper()
	r, _ := http.NewRequest(method, h.api.URL+path, strings.NewReader(body))
	r.Host = "127.0.0.1:17840"
	r.Header.Set("Authorization", "Bearer test-secret")
	return h.do(r)
}

func (h *citeHarness) do(r *http.Request) (int, string) {
	h.t.Helper()
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

// subscribe opens the Reader window's event stream and returns its events.
func (h *citeHarness) subscribe() (<-chan citeEvent, context.CancelFunc) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, "GET", h.api.URL+"/api/cite/events", nil)
	r.Host = "127.0.0.1:17840"
	r.Header.Set("Authorization", "Bearer test-secret")
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		h.t.Fatal(err)
	}
	events := make(chan citeEvent, 8)
	go func() {
		defer response.Body.Close()
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				var event citeEvent
				_ = json.Unmarshal([]byte(data), &event)
				events <- event
			}
		}
	}()
	h.waitFor(func() bool {
		h.server.cite.mu.Lock()
		defer h.server.cite.mu.Unlock()
		return len(h.server.cite.subscribers) > 0
	})
	return events, cancel
}

func (h *citeHarness) waitFor(ready func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			h.t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func next(t *testing.T, events <-chan citeEvent) citeEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
		return citeEvent{}
	}
}

type reply struct {
	status int
	body   string
}

func (h *citeHarness) async(call func() (int, string)) <-chan reply {
	done := make(chan reply, 1)
	go func() {
		status, body := call()
		done <- reply{status, body}
	}()
	return done
}

func wait(t *testing.T, done <-chan reply) reply {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish")
		return reply{}
	}
}

func TestCiteTransactionRelaysOperationsInOrder(t *testing.T) {
	h := newCiteHarness(t)
	events, cancel := h.subscribe()
	defer cancel()

	command := h.async(func() (int, string) {
		return h.client("/v1/cite/execCommand", `{"command":"addEditCitation","documentId":"doc-1","client":"word-mac"}`, h.secret)
	})
	event := next(t, events)
	if event.Type != "command" || event.Command != "addEditCitation" || event.DocumentID != "doc-1" || event.Client != "word-mac" {
		t.Fatalf("event %+v", event)
	}
	call := h.async(func() (int, string) {
		return h.reader("POST", "/api/cite/transactions/"+event.Transaction+"/call", `{"op":"Document.getState","transaction":"`+event.Transaction+`"}`)
	})
	if r := wait(t, command); r.status != 200 || !strings.Contains(r.body, "Document.getState") {
		t.Fatalf("command reply %+v", r)
	}
	respond := h.async(func() (int, string) {
		return h.client("/v1/cite/respond", `{"transaction":"`+event.Transaction+`","result":{"fields":[]}}`, h.secret)
	})
	if r := wait(t, call); r.status != 200 || r.body != `{"fields":[]}` {
		t.Fatalf("call reply %+v", r)
	}
	if status, body := h.reader("POST", "/api/cite/transactions/"+event.Transaction+"/complete", `{"activate":true}`); status != 204 {
		t.Fatalf("complete %d %s", status, body)
	}
	r := wait(t, respond)
	if r.status != 200 || !strings.Contains(r.body, `"op":"Document.complete"`) || !strings.Contains(r.body, `"activate":true`) {
		t.Fatalf("final reply %+v", r)
	}
	// The slot is free again.
	second := h.async(func() (int, string) {
		return h.client("/v1/cite/execCommand", `{"command":"refresh","documentId":"doc-1","client":"wps"}`, h.secret)
	})
	if event := next(t, events); event.Type != "command" || event.Command != "refresh" {
		t.Fatalf("second event %+v", event)
	}
	h.server.cite.mu.Lock()
	current := h.server.cite.current
	h.server.cite.mu.Unlock()
	h.server.abortCite(current)
	if r := wait(t, second); !strings.Contains(r.body, "Document.complete") {
		t.Fatalf("aborted reply %+v", r)
	}
}

func TestCiteClientErrorReachesReader(t *testing.T) {
	h := newCiteHarness(t)
	events, cancel := h.subscribe()
	defer cancel()
	command := h.async(func() (int, string) {
		return h.client("/v1/cite/execCommand", `{"command":"refresh","documentId":"d","client":"word-windows"}`, h.secret)
	})
	event := next(t, events)
	call := h.async(func() (int, string) {
		return h.reader("POST", "/api/cite/transactions/"+event.Transaction+"/call", `{"op":"Document.updateFields"}`)
	})
	wait(t, command)
	h.async(func() (int, string) {
		return h.client("/v1/cite/respond", `{"transaction":"`+event.Transaction+`","error":"文档处于只读状态"}`, h.secret)
	})
	if r := wait(t, call); r.status != 422 || !strings.Contains(r.body, "只读") {
		t.Fatalf("call reply %+v", r)
	}
}

func TestCiteRejectsConcurrentCommandsAndMissingWindow(t *testing.T) {
	h := newCiteHarness(t)
	body := `{"command":"refresh","documentId":"d","client":"word-windows"}`
	if status, _ := h.client("/v1/cite/execCommand", body, h.secret); status != 503 {
		t.Fatalf("without window: %d", status)
	}
	events, cancel := h.subscribe()
	defer cancel()
	first := h.async(func() (int, string) { return h.client("/v1/cite/execCommand", body, h.secret) })
	next(t, events)
	if status, _ := h.client("/v1/cite/execCommand", body, h.secret); status != 409 {
		t.Fatalf("concurrent: %d", status)
	}
	if event := next(t, events); event.Type != "focus" {
		t.Fatalf("expected focus event, got %+v", event)
	}
	// Closing the Reader window ends the open transaction.
	cancel()
	if r := wait(t, first); r.status != 200 || !strings.Contains(r.body, "Document.complete") {
		t.Fatalf("orphaned reply %+v", r)
	}
}

func TestCiteRequiresCredential(t *testing.T) {
	h := newCiteHarness(t)
	body := `{"command":"refresh","documentId":"d","client":"word-windows"}`
	for _, secret := range []string{"", "wrong", "test-secret"} {
		if status, _ := h.client("/v1/cite/execCommand", body, secret); status != 401 {
			t.Fatalf("secret %q: %d", secret, status)
		}
	}
	if status, _ := h.client("/v1/cite/execCommand", `{"command":"format","documentId":"d","client":"word-windows"}`, h.secret); status != 400 {
		t.Fatalf("unknown command: %d", status)
	}
	if status, _ := h.client("/v1/cite/respond", `{"transaction":"missing","result":1}`, h.secret); status != 410 {
		t.Fatalf("missing transaction: %d", status)
	}
	if status, _ := h.reader("POST", "/api/cite/transactions/missing/call", `{"op":"x"}`); status != 410 {
		t.Fatalf("missing transaction call: %d", status)
	}
}

func TestCiteCredentialIsPrivateAndRotates(t *testing.T) {
	s := testServer(t)
	path := filepath.Join(t.TempDir(), "Reader", "cite-credential")
	if err := s.SetCiteCredential(path); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	if err := s.SetCiteCredential(path); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if len(first) != 64 || string(first) == string(second) {
		t.Fatalf("credential did not rotate")
	}
}

func TestCiteServesWPSAddinWithoutCredential(t *testing.T) {
	h := newCiteHarness(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "wps", "js"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "wps", "index.html"), []byte(`<meta name="reader-cite" content="{{READER_CITE_CREDENTIAL}}">`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "wps", "js", "main.js"), []byte("ok"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("outside"), 0o644)
	h.server.SetCitePlugins(dir)
	get := func(path string) (int, string, string) {
		r, _ := http.NewRequest("GET", h.connector.URL+path, nil)
		r.Host = "127.0.0.1:" + ConnectorPort
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		return response.StatusCode, response.Header.Get("Content-Type"), string(data)
	}
	for _, path := range []string{"/wps/", "/wps/" + strings.Repeat("b", 64) + "/index.html"} {
		status, _, body := get(path)
		if status != 401 || strings.Contains(body, h.secret) {
			t.Fatalf("public access: %d", status)
		}
	}
	key := strings.Repeat("a", 64)
	if err := os.WriteFile(h.server.cite.bootstrapPath, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	base := "/wps/" + key + "/"
	status, kind, body := get(base + "index.html")
	if status != 200 || !strings.HasPrefix(kind, "text/html") || strings.Contains(body, h.secret) {
		t.Fatalf("index: %d %s %s", status, kind, body)
	}
	if status, kind, _ := get(base + "js/main.js"); status != 200 || !strings.HasPrefix(kind, "text/javascript") {
		t.Fatalf("script: %d %s", status, kind)
	}
	for _, path := range []string{"/wps/../secret.txt", "/wps/%2e%2e/secret.txt", "/wps/missing.js"} {
		if status, _, body := get(path); status == 200 || strings.Contains(body, "outside") {
			t.Fatalf("%s served: %d", path, status)
		}
	}
	// The page calls the citation endpoints from its own origin, never others.
	for origin, want := range map[string]int{"http://127.0.0.1:" + ConnectorPort: 503, "https://example.com": 403} {
		r, _ := http.NewRequest("POST", h.connector.URL+"/v1/cite/execCommand", strings.NewReader(`{"command":"refresh","documentId":"d","client":"wps"}`))
		r.Host = "127.0.0.1:" + ConnectorPort
		r.Header.Set("Origin", origin)
		r.Header.Set("Authorization", "Bearer "+h.secret)
		status, _ := h.do(r)
		if status != want {
			t.Fatalf("origin %s: %d", origin, status)
		}
	}
}

func TestCiteAsksIdleClientsToPollAgain(t *testing.T) {
	previous := citeWaitInterval
	citeWaitInterval = 50 * time.Millisecond
	t.Cleanup(func() { citeWaitInterval = previous })
	h := newCiteHarness(t)
	events, cancel := h.subscribe()
	defer cancel()
	command := h.async(func() (int, string) {
		return h.client("/v1/cite/execCommand", `{"command":"addEditCitation","documentId":"d","client":"word-windows"}`, h.secret)
	})
	event := next(t, events)
	r := wait(t, command)
	if r.status != 200 || !strings.Contains(r.body, `"op":"Document.wait"`) || !strings.Contains(r.body, event.Transaction) {
		t.Fatalf("idle reply %+v", r)
	}
	// The transaction survives the idle reply; the next poll gets the operation.
	poll := h.async(func() (int, string) {
		return h.client("/v1/cite/next", `{"transaction":"`+event.Transaction+`"}`, h.secret)
	})
	call := h.async(func() (int, string) {
		return h.reader("POST", "/api/cite/transactions/"+event.Transaction+"/call", `{"op":"Document.getState"}`)
	})
	if r := wait(t, poll); !strings.Contains(r.body, "Document.getState") {
		t.Fatalf("poll reply %+v", r)
	}
	h.async(func() (int, string) {
		return h.client("/v1/cite/respond", `{"transaction":"`+event.Transaction+`","result":{}}`, h.secret)
	})
	if r := wait(t, call); r.status != 200 {
		t.Fatalf("call reply %+v", r)
	}
	if status, body := h.client("/v1/cite/next", `{"transaction":"gone"}`, h.secret); status != 200 || !strings.Contains(body, "Document.complete") {
		t.Fatalf("ended transaction: %d %s", status, body)
	}
}

// The public page must not be an alternate route around the credential file.
func TestCiteBootstrapRequiresPrivateInstallationKey(t *testing.T) {
	h := newCiteHarness(t)
	bootstrap := "/v1/cite/bootstrap"
	for _, token := range []string{"", "wrong", h.secret} {
		if status, body := h.client(bootstrap, "{}", token); status != 401 || strings.Contains(body, h.secret) {
			t.Fatalf("missing installation key: status %d", status)
		}
	}
	key := strings.Repeat("a", 64)
	path := h.server.cite.bootstrapPath
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", strings.Repeat("b", 64), h.secret} {
		if status, body := h.client(bootstrap, "{}", token); status != 401 || strings.Contains(body, h.secret) {
			t.Fatalf("invalid installation key: status %d", status)
		}
	}
	status, body := h.client(bootstrap, "{}", key)
	if status != 200 || !strings.Contains(body, h.secret) {
		t.Fatalf("bootstrap: status %d", status)
	}
	// Installation key is scoped to bootstrap, not document operations.
	if status, _ := h.client("/v1/cite/execCommand", "{}", key); status != 401 {
		t.Fatalf("installation key used as runtime credential: %d", status)
	}
	// Runtime rotation does not require reinstalling WPS.
	if err := h.server.SetCiteCredential(filepath.Join(filepath.Dir(path), "cite-credential")); err != nil {
		t.Fatal(err)
	}
	status, body = h.client(bootstrap, "{}", key)
	if status != 200 || strings.Contains(body, h.secret) {
		t.Fatalf("rotation: status %d", status)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if status, _ := h.client(bootstrap, "{}", key); status != 401 {
		t.Fatalf("revocation: %d", status)
	}
}

func TestCiteWPSKeyDoesNotEnterDiagnosticsOrReferrers(t *testing.T) {
	h := newCiteHarness(t)
	logs, err := diagnostics.New(filepath.Join(h.server.Store.Root, "logs"), "wps-test")
	if err != nil {
		t.Fatal(err)
	}
	h.server.SetDiagnostics(logs)
	key := strings.Repeat("c", 64)
	if err := os.WriteFile(h.server.cite.bootstrapPath, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "wps"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wps", "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.server.SetCitePlugins(dir)
	req := httptest.NewRequest("GET", "http://127.0.0.1:"+ConnectorPort+"/wps/"+key+"/index.html", nil)
	response := httptest.NewRecorder()
	h.server.ConnectorHandler().ServeHTTP(response, req)
	if response.Code != 200 || response.Header().Get("Referrer-Policy") != "no-referrer" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("resource privacy headers: status %d", response.Code)
	}
	if status, _ := h.client("/v1/cite/bootstrap", "{}", key); status != 200 {
		t.Fatal(status)
	}
	entries, err := os.ReadDir(logs.Dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(logs.Dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if strings.Contains(text, key) || strings.Contains(text, h.secret) {
			t.Fatal("credentials leaked into diagnostics")
		}
		if strings.Contains(text, "http.finish") {
			found = true
		}
	}
	if !found {
		t.Fatal("missing HTTP diagnostics")
	}
}

const testExtensionOrigin = connectorExtensionOrigin

func TestConnectorStatusReason(t *testing.T) {
	s := testServer(t)
	for _, tc := range []struct {
		name      string
		reason    string
		available bool
	}{
		{"disabled", "disabled", false},
		{"port in use", "port-in-use", false},
		{"available", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.available {
				s.SetConnectorAvailable(true)
			} else {
				s.SetConnectorUnavailable(tc.reason)
			}
			w := request(t, s, "GET", "/api/connector/status", nil)
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			var status struct {
				Available bool   `json:"available"`
				Reason    string `json:"reason"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if status.Available != tc.available || status.Reason != tc.reason {
				t.Fatalf("unexpected status: %+v", status)
			}
		})
	}
}

func bridgeRequest(s *Server, method, path, secret string, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, "http://127.0.0.1:17841"+path, nil)
	} else {
		r = httptest.NewRequest(method, "http://127.0.0.1:17841"+path, body)
	}
	r.Header.Set("Origin", testExtensionOrigin)
	if secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	s.ConnectorHandler().ServeHTTP(w, r)
	return w
}
func sessionTestConnector(t *testing.T, s *Server) string {
	t.Helper()
	s.SetConnectorAvailable(true)
	w := bridgeRequest(s, "POST", "/v1/session", "", nil, "")
	if w.Code != 200 {
		t.Fatalf("session %d: %s", w.Code, w.Body.String())
	}
	var session struct {
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &session)
	return session.Secret
}
func TestConnectorSessionAndImport(t *testing.T) {
	s := testServer(t)
	if w := bridgeRequest(s, "GET", "/v1/folders", "", nil, ""); w.Code != 401 {
		t.Fatalf("unauthorized: %d", w.Code)
	}
	secret := sessionTestConnector(t, s)
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	_ = m.WriteField("metadata", `{"title":"Test connector paper","doi":"10.1000/test","url":"https://example.org/paper","itemType":"journal"}`)
	_ = m.WriteField("sourceUrl", "https://example.org/paper")
	_ = m.WriteField("tags", `["research"]`)
	_ = m.WriteField("folders", `["Web"]`)
	_ = m.WriteField("snapshot", `<article><h1>Saved</h1><script>alert(1)</script><img src="https://evil.example/a"><a href="javascript:alert(1)">bad</a><a href="/paper/related">related</a></article>`)
	f, _ := m.CreateFormFile("pdf", "paper.pdf")
	_, _ = f.Write(testPDF(2))
	_ = m.Close()
	w := bridgeRequest(s, "POST", "/v1/import", secret, &body, m.FormDataContentType())
	if w.Code != 201 {
		t.Fatalf("import %d: %s", w.Code, w.Body.String())
	}
	var result struct {
		Document Document `json:"document"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if result.Document.Title != "Test connector paper" || result.Document.Library != "papers" || len(result.Document.Tags) != 1 {
		t.Fatalf("import result: %+v", result.Document)
	}
	snapshot := request(t, s, "GET", "/pub/test-secret/"+result.Document.ID+"/snapshot.html", nil)
	if snapshot.Code != 200 || !strings.Contains(snapshot.Body.String(), "Saved") || strings.Contains(snapshot.Body.String(), "alert(1)") || strings.Contains(snapshot.Body.String(), "https://evil.example/a") {
		t.Fatalf("unsafe snapshot: %d %s", snapshot.Code, snapshot.Body.String())
	}
	if !strings.Contains(snapshot.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("snapshot missing CSP sandbox")
	}
	if !strings.Contains(snapshot.Body.String(), `href="https://example.org/paper/related"`) {
		t.Fatal("relative snapshot link was not made absolute")
	}
	manual := request(t, s, "PATCH", "/api/documents/"+result.Document.ID, strings.NewReader(`{"title":"My edited title","metadata":{"venue":"My venue"}}`))
	if manual.Code != 200 {
		t.Fatal(manual.Body.String())
	}
	var duplicate bytes.Buffer
	dm := multipart.NewWriter(&duplicate)
	_ = dm.WriteField("metadata", `{"title":"Site title changed","venue":"Site venue","url":"https://example.org/paper"}`)
	_ = dm.WriteField("sourceUrl", "https://example.org/paper")
	df, _ := dm.CreateFormFile("pdf", "paper.pdf")
	_, _ = df.Write(testPDF(2))
	_ = dm.Close()
	w = bridgeRequest(s, "POST", "/v1/import", secret, &duplicate, dm.FormDataContentType())
	if w.Code != 200 {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
	updated, err := s.Store.Document(result.Document.ID)
	if err != nil || updated.Title != "My edited title" || updated.Metadata.Venue != "My venue" {
		t.Fatalf("manual metadata overwritten: %+v %v", updated, err)
	}
	_ = sessionTestConnector(t, s)
	if w = bridgeRequest(s, "GET", "/v1/folders", secret, nil, ""); w.Code != 401 {
		t.Fatalf("rotated credential: %d", w.Code)
	}
}
func TestConnectorLabelsIncludeSavedEmptyCategoriesAndPaperTags(t *testing.T) {
	s := testServer(t)
	secret := sessionTestConnector(t, s)
	if w := bridgeRequest(s, "GET", "/v1/labels", "", nil, ""); w.Code != 401 {
		t.Fatalf("labels without credential: %d", w.Code)
	}
	w := request(t, s, "PUT", "/api/preferences/library", strings.NewReader(`{"papers":{"categories":["Empty","ML/NLP"]}}`))
	if w.Code != 200 {
		t.Fatalf("preferences: %d %s", w.Code, w.Body.String())
	}
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	_ = m.WriteField("metadata", `{"title":"Tagged paper","url":"https://example.org/tagged"}`)
	_ = m.WriteField("sourceUrl", "https://example.org/tagged")
	_ = m.WriteField("folders", `["ML/NLP","Reading"]`)
	_ = m.WriteField("tags", `["Security","Web"]`)
	f, _ := m.CreateFormFile("pdf", "paper.pdf")
	_, _ = f.Write(testPDF(2))
	_ = m.Close()
	w = bridgeRequest(s, "POST", "/v1/import", secret, &body, m.FormDataContentType())
	if w.Code != 201 {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	w = bridgeRequest(s, "GET", "/v1/labels", secret, nil, "")
	if w.Code != 200 {
		t.Fatalf("labels: %d %s", w.Code, w.Body.String())
	}
	var labels struct {
		Folders []string `json:"folders"`
		Tags    []string `json:"tags"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &labels); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(labels.Folders, ","); got != "Empty,ML,ML/NLP,Reading" {
		t.Fatalf("folders: %q", got)
	}
	if got := strings.Join(labels.Tags, ","); got != "Security,Web" {
		t.Fatalf("tags: %q", got)
	}
}
func TestConnectorOriginRestriction(t *testing.T) {
	s := testServer(t)
	s.SetConnectorAvailable(true)
	r := httptest.NewRequest("GET", "http://127.0.0.1:17841/v1/status", nil)
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	s.ConnectorHandler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("web origin: %d", w.Code)
	}
	r.Header.Set("Origin", "chrome-extension://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	w = httptest.NewRecorder()
	s.ConnectorHandler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("other extension origin: %d", w.Code)
	}
	for _, origin := range []string{connectorExtensionOrigin, connectorStoreExtensionOrigin} {
		r.Header.Set("Origin", origin)
		w = httptest.NewRecorder()
		s.ConnectorHandler().ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != origin {
			t.Fatalf("%s: %d %q", origin, w.Code, w.Header().Get("Access-Control-Allow-Origin"))
		}
	}
}

func TestConnectorOriginlessBrowserRequestNeedsCredential(t *testing.T) {
	s := testServer(t)
	secret := sessionTestConnector(t, s)
	requestWithoutOrigin := func(path, credential string) int {
		r := httptest.NewRequest("GET", "http://127.0.0.1:17841"+path, nil)
		if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
		}
		w := httptest.NewRecorder()
		s.ConnectorHandler().ServeHTTP(w, r)
		return w.Code
	}
	if code := requestWithoutOrigin("/v1/folders", secret); code != 200 {
		t.Fatalf("Arc extension omits Origin on authenticated GET: %d", code)
	}
	if code := requestWithoutOrigin("/v1/labels", secret); code != 200 {
		t.Fatalf("Arc extension omits Origin on authenticated labels GET: %d", code)
	}
	if code := requestWithoutOrigin("/v1/folders", ""); code != 401 {
		t.Fatalf("originless request without credential: %d", code)
	}
	if code := requestWithoutOrigin("/v1/session", ""); code != 403 {
		t.Fatalf("originless session request: %d", code)
	}
}

func TestConnectorLargePaperConfirmation(t *testing.T) {
	lowerLargePaperBytes(t)
	for _, library := range []string{"papers", "books"} {
		t.Run(library, func(t *testing.T) {
			s := testServer(t)
			secret := sessionTestConnector(t, s)
			for _, confirmed := range []bool{false, true} {
				var body bytes.Buffer
				m := multipart.NewWriter(&body)
				m.WriteField("metadata", `{"title":"Large PDF"}`)
				m.WriteField("sourceUrl", "https://example.org/paper")
				if confirmed {
					m.WriteField("allowLarge", "1")
					m.WriteField("library", library)
				}
				f, _ := m.CreateFormFile("pdf", "paper.pdf")
				f.Write(testPDF(160))
				m.Close()
				w := bridgeRequest(s, "POST", "/v1/import", secret, &body, m.FormDataContentType())
				if !confirmed {
					if w.Code != 409 || !strings.Contains(w.Body.String(), "large-paper") {
						t.Fatalf("missing confirmation: %d %s", w.Code, w.Body.String())
					}
				} else {
					var result struct {
						Document Document `json:"document"`
					}
					if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Document.Library != library {
						t.Fatalf("destination: %d %s", w.Code, w.Body.String())
					}
				}
			}
		})
	}
}
