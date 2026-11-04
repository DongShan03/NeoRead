package neoread

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"neoread.local/server/internal/diagnostics"
	"neoread.local/server/internal/runtimeenv"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	store, e := OpenStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { store.DB.Close() })
	server := NewServer(store, "test-secret", "")
	t.Cleanup(server.Close)
	return server
}
func request(t *testing.T, s *Server, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://127.0.0.1:17840"+path, body)
	r.Header.Set("Authorization", "Bearer test-secret")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func upload(t *testing.T, s *Server, name string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
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

func uploadToFolder(t *testing.T, s *Server, name string, data []byte, library, folder string, smartID ...string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	_ = m.WriteField("library", library)
	_ = m.WriteField("folder", folder)
	if len(smartID) > 0 {
		_ = m.WriteField("smartCategory", smartID[0])
	}
	f, _ := m.CreateFormFile("file", name)
	_, _ = f.Write(data)
	_ = m.Close()
	r := httptest.NewRequest("POST", "http://127.0.0.1:17840/api/documents", &body)
	r.Header.Set("Authorization", "Bearer test-secret")
	r.Header.Set("Content-Type", m.FormDataContentType())
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestImportPaperIntoFolderKeepsExistingAssignment(t *testing.T) {
	s := testServer(t)
	data := sample(t, "reading-notes.pdf")
	first := uploadToFolder(t, s, "reading-notes.pdf", data, "papers", "Security/Web")
	if first.Code != 201 {
		t.Fatalf("import: %d %s", first.Code, first.Body.String())
	}
	var doc Document
	if err := json.Unmarshal(first.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Store.Document(doc.ID)
	if err != nil || len(saved.Folders) != 1 || saved.Folders[0] != "Security/Web" {
		t.Fatalf("folder not persisted: %+v %v", saved.Folders, err)
	}
	duplicate := uploadToFolder(t, s, "reading-notes.pdf", data, "papers", "Other")
	if duplicate.Code != 200 {
		t.Fatalf("duplicate: %d %s", duplicate.Code, duplicate.Body.String())
	}
	saved, err = s.Store.Document(doc.ID)
	if err != nil || len(saved.Folders) != 1 || saved.Folders[0] != "Security/Web" {
		t.Fatalf("duplicate changed folders: %+v %v", saved.Folders, err)
	}
	for _, folder := range []string{"Security/", " Security", strings.Repeat("x", 41)} {
		invalid := uploadToFolder(t, s, "reading-notes.pdf", data, "papers", folder)
		if invalid.Code != 400 {
			t.Fatalf("invalid folder %q: %d %s", folder, invalid.Code, invalid.Body.String())
		}
	}
	wrongLibrary := uploadToFolder(t, s, "reading-notes.pdf", data, "books", "Security")
	if wrongLibrary.Code != 400 {
		t.Fatalf("book folder: %d %s", wrongLibrary.Code, wrongLibrary.Body.String())
	}
}

func TestReimportKeepsExistingFilesWhenDocumentCannotBeRead(t *testing.T) {
	s := testServer(t)
	data := testPDF(2)
	first := uploadToFolder(t, s, "paper.pdf", data, "papers", "")
	if first.Code != 201 {
		t.Fatalf("import: %d %s", first.Code, first.Body.String())
	}
	var doc Document
	if err := json.Unmarshal(first.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(s.Store.Root, "cache", doc.ID)
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(cache, "preserved.txt")
	if err := os.WriteFile(marker, []byte("saved cache"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec("UPDATE documents SET metadata='invalid-json' WHERE id=?", doc.ID); err != nil {
		t.Fatal(err)
	}
	again := uploadToFolder(t, s, "paper.pdf", data, "papers", "")
	if again.Code != 500 {
		t.Fatalf("unreadable existing record: %d %s", again.Code, again.Body.String())
	}
	if saved, err := os.ReadFile(s.Store.File(doc)); err != nil || !bytes.Equal(saved, data) {
		t.Fatalf("failed reimport changed the original PDF: %v", err)
	}
	if saved, err := os.ReadFile(marker); err != nil || string(saved) != "saved cache" {
		t.Fatalf("failed reimport changed the existing cache: %v", err)
	}
}

func TestImportSmartCategoryTags(t *testing.T) {
	for _, source := range []string{"file", "reference"} {
		t.Run(source, func(t *testing.T) {
			s := testServer(t)
			scholarFixture(t, s)
			prefs := `{"papers":{"smartCategories":[{"id":"web","name":"Web Security","tags":["Security","Web","security"]}]}}`
			w := request(t, s, "PUT", "/api/preferences/library", strings.NewReader(prefs))
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			if source == "file" {
				w = uploadToFolder(t, s, "paper.pdf", testPDF(2), "papers", "", "web")
			} else {
				w = request(t, s, "POST", "/api/documents/resolve", strings.NewReader(`{"ref":"1706.03762","smartCategory":"web"}`))
			}
			if w.Code != 201 {
				t.Fatalf("import: %d %s", w.Code, w.Body.String())
			}
			var doc Document
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			saved, err := s.Store.Document(doc.ID)
			if err != nil || len(saved.Tags) != 2 || saved.Tags[0] != "Security" || saved.Tags[1] != "Web" || len(saved.Folders) != 0 {
				t.Fatalf("smart category tags not persisted: %+v %v", saved, err)
			}
		})
	}
}

func TestInvalidSmartCategoryDoesNotImport(t *testing.T) {
	s := testServer(t)
	request(t, s, "PUT", "/api/preferences/library", strings.NewReader(`{"papers":{"smartCategories":[{"id":"empty","tags":[]}]}}`))
	for _, smartID := range []string{"missing", "empty"} {
		w := uploadToFolder(t, s, "paper.pdf", testPDF(2), "papers", "", smartID)
		if w.Code != 400 {
			t.Fatalf("invalid smart category: %d %s", w.Code, w.Body.String())
		}
	}
	var count int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM documents").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed import saved a document: count=%d error=%v", count, err)
	}
}
func sample(t *testing.T, name string) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("../../../web/public/samples", name))
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestLibraryRoundTrip(t *testing.T) {
	s := testServer(t)
	for _, name := range []string{"the-art-of-reading.epub", "reading-notes.pdf"} {
		t.Run(name, func(t *testing.T) {
			data := sample(t, name)
			w := upload(t, s, name, data)
			if w.Code != 201 {
				t.Fatalf("import: %d %s", w.Code, w.Body.String())
			}
			var doc Document
			if e := json.Unmarshal(w.Body.Bytes(), &doc); e != nil {
				t.Fatal(e)
			}
			if doc.LastOpenedAt != "" {
				t.Fatalf("unopened import has last opened time: %s", doc.LastOpenedAt)
			}
			if duplicate := upload(t, s, name, data); duplicate.Code != 200 {
				t.Fatalf("duplicate: %d", duplicate.Code)
			}
			progress := `{"type":"pdf","page":2}`
			if doc.Type == "epub" {
				progress = `{"type":"epub","href":"EPUB/chapter2.xhtml","progression":0.4,"locator":"{}"}`
			}
			w = request(t, s, "PATCH", "/api/documents/"+doc.ID, strings.NewReader(`{"favorite":true,"progress":`+progress+`,"percentage":0.5}`))
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			updated, err := s.Store.Document(doc.ID)
			if err != nil || updated.LastOpenedAt == "" || !updated.Favorite || updated.Percentage != .5 || !bytes.Equal(updated.Progress, []byte(progress)) {
				t.Fatalf("progress not persisted: %+v %v", updated, err)
			}
			ann := `{"kind":"note","location":` + progress + `,"quote":"evidence","note":"Check assumptions","color":"#ffff00"}`
			w = request(t, s, "POST", "/api/documents/"+doc.ID+"/annotations", strings.NewReader(ann))
			if w.Code != 201 {
				t.Fatal(w.Body.String())
			}
			var a Annotation
			json.Unmarshal(w.Body.Bytes(), &a)
			w = request(t, s, "GET", "/api/documents/"+doc.ID+"/annotations", nil)
			if !strings.Contains(w.Body.String(), "Check assumptions") {
				t.Fatal(w.Body.String())
			}
			if doc.Type == "epub" {
				w = request(t, s, "GET", "/pub/test-secret/"+doc.ID+"/manifest.json", nil)
				if w.Code != 200 || !strings.Contains(w.Body.String(), "toc") {
					t.Fatalf("manifest: %s", w.Body.String())
				}
				w = request(t, s, "GET", "/pub/test-secret/"+doc.ID+"/EPUB/chapter1.xhtml", nil)
				if w.Code != 200 || !strings.Contains(w.Body.String(), "阅读之前") {
					t.Fatalf("resource: %s", w.Body.String())
				}
				decoder := xml.NewDecoder(strings.NewReader(w.Body.String()))
				for {
					_, e := decoder.Token()
					if e == io.EOF {
						break
					}
					if e != nil {
						t.Fatal(e)
					}
				}
				w = request(t, s, "GET", "/api/documents/"+doc.ID+"/search?q=NeoRead", nil)
				if w.Code != 200 {
					t.Fatal(w.Body.String())
				}
				w = request(t, s, "GET", "/pub/test-secret/"+doc.ID+"/positions.json", nil)
				if !strings.Contains(w.Body.String(), "progression") {
					t.Fatal("missing positions")
				}
			} else {
				r := httptest.NewRequest("GET", "http://127.0.0.1:17840/pub/test-secret/"+doc.ID+"/original.pdf", nil)
				r.Header.Set("Range", "bytes=0-4")
				w = httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				if w.Code != 206 || w.Body.String() != "%PDF-" {
					t.Fatalf("range: %d %s", w.Code, w.Body.String())
				}
			}
			w = request(t, s, "DELETE", "/api/documents/"+doc.ID+"/annotations/"+a.ID, nil)
			if w.Code != 204 {
				t.Fatal(w.Code)
			}
		})
	}
	docs, e := s.Store.Documents()
	if e != nil || len(docs) != 2 {
		t.Fatalf("library: %v %v", docs, e)
	}
}
func TestEPUBPositionsIncludeTotal(t *testing.T) {
	s := testServer(t)
	w := upload(t, s, "the-art-of-reading.epub", sample(t, "the-art-of-reading.epub"))
	if w.Code != 201 {
		t.Fatalf("import: %s", w.Body.String())
	}
	var doc Document
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(s.Store.Root, "cache", doc.ID, "positions.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var cached struct {
		Total     int               `json:"total"`
		Positions []json.RawMessage `json:"positions"`
	}
	if err := json.Unmarshal(data, &cached); err != nil {
		t.Fatal(err)
	}
	if cached.Total == 0 || cached.Total != len(cached.Positions) {
		t.Errorf("invalid cached position count: %s", data)
	}
	// Previously imported books have no total. Serve them without reimporting or
	// changing their persisted progress and annotations.
	legacy, err := json.Marshal(map[string]any{"positions": cached.Positions})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	w = request(t, s, "GET", "/pub/test-secret/"+doc.ID+"/positions.json", nil)
	var served struct {
		Total     int               `json:"total"`
		Positions []json.RawMessage `json:"positions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &served); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || served.Total == 0 || served.Total != len(served.Positions) {
		t.Fatalf("invalid served position count: %s", w.Body.String())
	}
	if len(served.Positions) != len(cached.Positions) {
		t.Fatal("positions changed")
	}
}

func TestAuthBoundaries(t *testing.T) {
	s := testServer(t)
	for _, test := range []struct {
		name, host, origin, token string
		want                      int
	}{{"missing token", "127.0.0.1:17840", "", "", 401}, {"foreign origin", "127.0.0.1:17840", "https://evil.example", "test-secret", 403}, {"rebinding", "evil.example:17840", "", "test-secret", 403}, {"valid", "127.0.0.1:17840", "http://127.0.0.1:17840", "test-secret", 200}} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://"+test.host+"/api/documents", nil)
			r.Header.Set("Authorization", "Bearer "+test.token)
			r.Header.Set("Origin", test.origin)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != test.want {
				t.Fatal(w.Code)
			}
		})
	}
}
func TestRejectInvalidImports(t *testing.T) {
	s := testServer(t)
	if w := upload(t, s, "bad.pdf", []byte("not pdf")); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := upload(t, s, "bad.epub", []byte("not zip")); w.Code != 400 {
		t.Fatal(w.Code)
	}
	var data bytes.Buffer
	z := zip.NewWriter(&data)
	f, _ := z.Create("../../escape")
	f.Write([]byte("unsafe"))
	z.Close()
	if w := upload(t, s, "traversal.epub", data.Bytes()); w.Code != 400 {
		t.Fatal(w.Code)
	}
	docs, _ := s.Store.Documents()
	if len(docs) != 0 {
		t.Fatal("invalid imports persisted")
	}
}
func TestContentSanitization(t *testing.T) {
	out, text, e := sanitizeContent([]byte(`<html xmlns="http://www.w3.org/1999/xhtml"><head><style>p{color:red}</style><script>secret()</script></head><body onload="secret()"><p id="target">Safe<br/>text &amp; notes</p><a href="java&#x09;script:evil()">link</a><iframe srcdoc="evil"/></body></html>`))
	if e != nil {
		t.Fatal(e)
	}
	for _, unsafe := range []string{"<script", "onload", "javascript", "iframe", "srcdoc"} {
		if strings.Contains(string(out), unsafe) {
			t.Fatalf("unsafe %s: %s", unsafe, out)
		}
	}
	if !strings.Contains(string(out), "<style>") || !strings.Contains(text, "Safe") {
		t.Fatal(string(out))
	}
	d := xml.NewDecoder(bytes.NewReader(out))
	for {
		_, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}
func TestLocationValidation(t *testing.T) {
	for _, bad := range []string{`{"type":"epub","href":"../secret"}`, `{"type":"epub","href":"https://evil.test/file"}`, `{"type":"epub","href":"chapter.xhtml","progression":2}`} {
		if validLocation([]byte(bad), "epub") {
			t.Fatal(bad)
		}
	}
	if validLocation([]byte(`{"type":"pdf","page":0}`), "pdf") {
		t.Fatal("zero page")
	}
}
func TestClaudeEventParsing(t *testing.T) {
	part, done, bad := claudeEvent([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"你好"}}}`))
	if part != "你好" || done || bad {
		t.Fatal(part, done, bad)
	}
	part, _, _ = claudeEvent([]byte(`{"type":"stream_event","parent_tool_use_id":"child","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"private"}}}`))
	if part != "" {
		t.Fatal("child message leaked")
	}
	_, done, bad = claudeEvent([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true}`))
	if !bad || done {
		t.Fatal("failure ignored")
	}
}
func TestStoreReopen(t *testing.T) {
	root := t.TempDir()
	s, e := OpenStore(root)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DB.Exec("INSERT INTO settings VALUES('neoread','{\"mode\":\"dark\"}')")
	if e != nil {
		t.Fatal(e)
	}
	s.DB.Close()
	s, e = OpenStore(root)
	if e != nil {
		t.Fatal(e)
	}
	defer s.DB.Close()
	var value string
	if e = s.DB.QueryRow("SELECT value FROM settings").Scan(&value); e != nil || !strings.Contains(value, "dark") {
		t.Fatal(value, e)
	}
}
func TestProviderMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if providerStatus(context.Background(), "codex").Installed {
		t.Fatal("reported absent CLI as installed")
	}
}
func TestChatWithFakeCLI(t *testing.T) {
	s := testServer(t)
	fakeAgent(t, "codex", "success")
	capture := filepath.Join(t.TempDir(), "prompt.json")
	t.Setenv("NEOREAD_AGENT_CAPTURE", capture)
	w := upload(t, s, "paper.pdf", sample(t, "reading-notes.pdf"))
	var d Document
	json.Unmarshal(w.Body.Bytes(), &d)
	w = request(t, s, "POST", "/api/documents/"+d.ID+"/chat", strings.NewReader(`{"provider":"codex","prompt":"Explain","context":"Some text"}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "event: done") || !strings.Contains(w.Body.String(), "你好") || !strings.Contains(w.Body.String(), "，世界") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(t, s, "GET", "/api/documents/"+d.ID+"/messages", nil)
	var messages []Message
	json.Unmarshal(w.Body.Bytes(), &messages)
	if len(messages) != 2 {
		t.Fatal(w.Body.String())
	}
	if messages[0].Context != "Some text" {
		t.Fatal("source excerpts were not persisted")
	}
	w = request(t, s, "POST", "/api/documents/"+d.ID+"/chat", strings.NewReader(`{"provider":"codex","prompt":"Explain the previous excerpt","context":"A new page"}`))
	if !strings.Contains(w.Body.String(), "event: done") {
		t.Fatal(w.Body.String())
	}
	prompt, e := os.ReadFile(capture)
	if e != nil || strings.Count(string(prompt), "Some text") != 1 || !strings.Contains(string(prompt), "Explain") || !strings.Contains(string(prompt), "A new page") {
		t.Fatal("prior context missing from follow-up", e)
	}

}

func TestEPUBDeclaredMediaTypeSanitized(t *testing.T) {
	s := testServer(t)
	original := sample(t, "the-art-of-reading.epub")
	source, e := zip.NewReader(bytes.NewReader(original), int64(len(original)))
	if e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	for _, file := range source.File {
		r, _ := file.Open()
		body, _ := io.ReadAll(r)
		r.Close()
		name := file.Name
		if name == "EPUB/package.opf" || name == "EPUB/nav.xhtml" {
			body = bytes.ReplaceAll(body, []byte("chapter1.xhtml"), []byte("chapter1.xml"))
		}
		if name == "EPUB/package.opf" {
			body = bytes.Replace(body, []byte("</manifest>"), []byte(`<item id="alias" href="chapter1.xml" media-type="text/plain"/></manifest>`), 1)
		}
		if name == "EPUB/chapter1.xhtml" {
			name = "EPUB/chapter1.xml"
			body = bytes.Replace(body, []byte("<body>"), []byte(`<body onload="evil()"><script>parent.stolen=true</script>`), 1)
		}
		w, _ := writer.Create(name)
		w.Write(body)
	}
	writer.Close()
	response := upload(t, s, "mismatch.epub", out.Bytes())
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	var d Document
	json.Unmarshal(response.Body.Bytes(), &d)
	response = request(t, s, "GET", "/pub/test-secret/"+d.ID+"/EPUB/chapter1.xml", nil)
	if response.Code != 200 || strings.Contains(response.Body.String(), "<script") || strings.Contains(response.Body.String(), "onload") {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request(t, s, "GET", "/api/documents/"+d.ID+"/search?q="+url.QueryEscape("阅读之前"), nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "chapter1.xml") {
		t.Fatal("CJK search:", response.Body.String())
	}
}
func TestSVGRemainsSVG(t *testing.T) {
	out, _, e := sanitizeContent([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><circle cx="5" cy="5" r="4"/><script>evil()</script></svg>`))
	if e != nil || !strings.HasPrefix(string(out), "<svg") || strings.Contains(string(out), "<script") {
		t.Fatal(string(out), e)
	}
}

func TestReaderPolicyAllowsReadiumBlobStyles(t *testing.T) {
	s := testServer(t)
	w := request(t, s, "GET", "/api/health", nil)
	directives := map[string][]string{}
	for _, directive := range strings.Split(w.Header().Get("Content-Security-Policy"), ";") {
		fields := strings.Fields(directive)
		if len(fields) > 1 {
			directives[fields[0]] = fields[1:]
		}
	}
	// Blob chapter frames inherit the creator's policy. Readium's CSS is also
	// a blob stylesheet, so permitting blob frames alone does not enable it.
	for _, directive := range []string{"style-src", "frame-src"} {
		found := false
		for _, source := range directives[directive] {
			if source == "blob:" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s blocks Readium blob resources", directive)
		}
	}
	for _, directive := range []string{"default-src", "connect-src"} {
		if got := strings.Join(directives[directive], " "); got != "'self'" {
			t.Errorf("%s unexpectedly widened: %s", directive, got)
		}
	}
	if got := strings.Join(directives["object-src"], " "); got != "'none'" {
		t.Errorf("object-src unexpectedly widened: %s", got)
	}
}
func TestWebAssetsKeepModuleMIMETypes(t *testing.T) {
	// Windows MIME registrations can map ".mjs" to "text/plain", and Chromium
	// then refuses to load the ES module that starts the PDF.js worker.
	mime.AddExtensionType(".mjs", "text/plain; charset=utf-8")
	web := t.TempDir()
	for name, content := range map[string]string{"page.html": "<!doctype html>", "app.js": "export const ok = 1;", "app.mjs": "export const ok = 1;", "app.css": "body { margin: 0; }"} {
		if e := os.WriteFile(filepath.Join(web, name), []byte(content), 0o600); e != nil {
			t.Fatal(e)
		}
	}
	store, e := OpenStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { store.DB.Close() })
	server := NewServer(store, "test-secret", web)
	t.Cleanup(server.Close)
	for path, want := range map[string]string{"/app.mjs": "text/javascript; charset=utf-8", "/app.js": "text/javascript; charset=utf-8", "/app.css": "text/css; charset=utf-8", "/page.html": "text/html; charset=utf-8"} {
		response := request(t, server, "GET", path, nil)
		if response.Code != 200 || response.Header().Get("Content-Type") != want {
			t.Errorf("%s: status %d, Content-Type %q, want %q", path, response.Code, response.Header().Get("Content-Type"), want)
		}
	}
}

func TestOpeningImportRecordsTimeWithoutChangingAddedTime(t *testing.T) {
	s := testServer(t)
	w := upload(t, s, "reading-notes.pdf", sample(t, "reading-notes.pdf"))
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var doc Document
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	w = request(t, s, "PATCH", "/api/documents/"+doc.ID, strings.NewReader(`{"title":"Renamed"}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	renamed, err := s.Store.Document(doc.ID)
	if err != nil || renamed.LastOpenedAt != "" {
		t.Fatalf("rename marked import as opened: %+v %v", renamed, err)
	}
	w = request(t, s, "PATCH", "/api/documents/"+doc.ID, strings.NewReader(`{}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	opened, err := s.Store.Document(doc.ID)
	if err != nil || opened.LastOpenedAt == "" || opened.CreatedAt != doc.CreatedAt {
		t.Fatalf("incorrect open dates: %+v %v", opened, err)
	}
}

func TestDiagnosticsAuthenticationAndRequestCorrelation(t *testing.T) {
	s := testServer(t)
	logs, _ := diagnostics.New(filepath.Join(s.Store.Root, "logs"), "session", "test-secret")
	s.SetDiagnostics(logs)
	denied := httptest.NewRecorder()
	s.Handler().ServeHTTP(denied, httptest.NewRequest("GET", "http://127.0.0.1:17840/api/diagnostics/export", nil))
	if denied.Code != 401 {
		t.Fatal(denied.Code)
	}
	first := request(t, s, "GET", "/api/documents", nil)
	rid := first.Header().Get("X-NeoRead-Request-ID")
	if len(rid) != 32 {
		t.Fatal("missing request ID")
	}
	w := request(t, s, "POST", "/api/diagnostics/events", strings.NewReader(`{"level":"info","event":"api.finish","requestId":"`+rid+`","status":200}`))
	if w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	w = request(t, s, "POST", "/api/diagnostics/events", strings.NewReader(`{"level":"error","event":"bad","body":"PRIVATE"}`))
	if w.Code != 400 {
		t.Fatal("accepted unknown body")
	}
	bundle := request(t, s, "GET", "/api/diagnostics/export", nil)
	if bundle.Code != 200 {
		t.Fatal(bundle.Body.String())
	}
	z, err := zip.NewReader(bytes.NewReader(bundle.Body.Bytes()), int64(bundle.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, f := range z.File {
		r, _ := f.Open()
		b, _ := io.ReadAll(r)
		r.Close()
		all.Write(b)
	}
	if !strings.Contains(all.String(), rid) || !strings.Contains(all.String(), "renderer.api.finish") || strings.Contains(all.String(), "test-secret") {
		t.Fatal("missing correlation or leaked credential")
	}
}

func TestDiagnosticsWrapperPreservesFlushAndCancellation(t *testing.T) {
	s := testServer(t)
	recorder := httptest.NewRecorder()
	s.diagnosticHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestID(r.Context()) == "" {
			t.Fatal("missing context ID")
		}
		io.WriteString(w, "event: delta\ndata: {}\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Fatal(err)
		}
	})).ServeHTTP(recorder, httptest.NewRequest("GET", "http://localhost/stream", nil))
	if !recorder.Flushed || !strings.Contains(recorder.Body.String(), "delta") {
		t.Fatal("stream wrapper broke Flush")
	}
}

func TestDiagnosticsHTTPFailureKeepsPrivateMessageInResponseOnly(t *testing.T) {
	s := testServer(t)
	logs, err := diagnostics.New(filepath.Join(s.Store.Root, "logs"), "session")
	if err != nil {
		t.Fatal(err)
	}
	s.SetDiagnostics(logs)
	// Scholar failures can contain private titles and bare hosts, neither of
	// which can safely be identified by a generic URL/credential redactor.
	message := "内部论文 Unpublished Project Nimbus 无法下载；无法连接 papers.private.corp"
	w := httptest.NewRecorder()
	s.diagnosticHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusBadGateway, message)
	})).ServeHTTP(w, httptest.NewRequest("POST", "http://localhost/api/documents/resolve", nil))
	var response map[string]string
	if w.Code != 502 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response["error"] != message {
		t.Fatal("user-visible failure response changed")
	}
	entries, err := os.ReadDir(logs.Dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(logs.Dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "Nimbus") || strings.Contains(string(data), "papers.private.corp") || strings.Contains(string(data), "userMessage") {
			t.Fatal("user-visible message entered persistent logs")
		}
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			var event map[string]any
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatal(err)
			}
			if event["event"] == "http.failure" {
				found = true
				if event["errorCode"] != "http_502" || event["requestId"] != w.Header().Get("X-NeoRead-Request-ID") {
					t.Fatal("failure lost its code or request correlation")
				}
			}
		}
	}
	if !found {
		t.Fatal("failure event missing")
	}
}

func TestLongPaperAsksBeforeImport(t *testing.T) {
	lowerLargePaperBytes(t)
	s := testServer(t)
	w := uploadTo(t, s, "papers", "long.pdf", testPDF(largePaperPages+10))
	var body struct {
		Code  string `json:"code"`
		Pages int    `json:"pages"`
	}
	if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Code != "large-paper" || body.Pages != largePaperPages+10 {
		t.Fatalf("long paper imported without asking: %d %s", w.Code, w.Body.String())
	}
	if docs, _ := s.Store.Documents(); len(docs) != 0 {
		t.Fatal("unconfirmed long paper was saved")
	}
	var buf bytes.Buffer
	m := multipart.NewWriter(&buf)
	_ = m.WriteField("library", "papers")
	_ = m.WriteField("allowLarge", "1")
	f, _ := m.CreateFormFile("file", "long.pdf")
	f.Write(testPDF(largePaperPages + 10))
	m.Close()
	r := httptest.NewRequest("POST", "http://127.0.0.1:17840/api/documents", &buf)
	r.Header.Set("Authorization", "Bearer test-secret")
	r.Header.Set("Content-Type", m.FormDataContentType())
	confirmed := httptest.NewRecorder()
	s.Handler().ServeHTTP(confirmed, r)
	if confirmed.Code != 201 {
		t.Fatalf("confirmed long paper: %d %s", confirmed.Code, confirmed.Body.String())
	}
	if w := uploadTo(t, s, "papers", "short.pdf", testPDF(largePaperPages)); w.Code != 201 {
		t.Fatalf("a %d-page paper asked: %s", largePaperPages, w.Body.String())
	}
}

func TestQuestionAnnotationsTrackAnswers(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	w := request(t, s, "POST", "/api/documents/"+d.ID+"/annotations", strings.NewReader(`{"kind":"question","location":{"type":"pdf","page":1},"quote":"why?","note":"Why does this hold?","color":"#e6b94c"}`))
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var q Annotation
	_ = json.Unmarshal(w.Body.Bytes(), &q)
	if got, _ := s.Store.Document(d.ID); got.OpenQuestionCount != 1 {
		t.Fatalf("open questions: %d", got.OpenQuestionCount)
	}
	path := "/api/documents/" + d.ID + "/annotations/" + q.ID
	if w = request(t, s, "PATCH", path, strings.NewReader(`{"answerId":"missing"}`)); w.Code != 400 {
		t.Fatalf("unknown answer accepted: %d", w.Code)
	}
	if err := s.Store.saveMessage(Message{DocumentID: d.ID, Role: "assistant", Content: "Because."}); err != nil {
		t.Fatal(err)
	}
	var message string
	s.Store.DB.QueryRow("SELECT id FROM messages WHERE document_id=?", d.ID).Scan(&message)
	if w = request(t, s, "PATCH", path, strings.NewReader(`{"answerId":"`+message+`"}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &q)
	if q.AnswerID != message || q.Note != "Why does this hold?" || q.Resolved {
		t.Fatalf("answered: %+v", q)
	}
	if got, _ := s.Store.Document(d.ID); got.OpenQuestionCount != 0 {
		t.Fatalf("answered question still open: %d", got.OpenQuestionCount)
	}
	// Clearing the answer reopens it; resolving closes it without an answer.
	request(t, s, "PATCH", path, strings.NewReader(`{"answerId":""}`))
	if got, _ := s.Store.Document(d.ID); got.OpenQuestionCount != 1 {
		t.Fatal("cleared answer did not reopen the question")
	}
	w = request(t, s, "PATCH", path, strings.NewReader(`{"resolved":true}`))
	_ = json.Unmarshal(w.Body.Bytes(), &q)
	if !q.Resolved {
		t.Fatal(w.Body.String())
	}
	// A note edit keeps the resolved flag as a JSON boolean.
	w = request(t, s, "PATCH", path, strings.NewReader(`{"note":"Edited"}`))
	if err := json.Unmarshal(w.Body.Bytes(), &q); err != nil || !q.Resolved || q.Note != "Edited" {
		t.Fatalf("note edit: %+v %v %s", q, err, w.Body.String())
	}
	if got, _ := s.Store.Document(d.ID); got.OpenQuestionCount != 0 {
		t.Fatal("resolved question counted as open")
	}
	// Underline compaction re-encodes every annotation of the document.
	for _, x := range []string{"0.1", "0.15"} {
		request(t, s, "POST", "/api/documents/"+d.ID+"/annotations", strings.NewReader(`{"kind":"underline","location":{"type":"pdf","page":1,"rects":[{"x":`+x+`,"y":0.1,"width":0.2,"height":0.02}]},"quote":"a","note":"","color":"#e6b94c"}`))
	}
	w = request(t, s, "GET", "/api/documents/"+d.ID+"/annotations", nil)
	if !strings.Contains(w.Body.String(), `"resolved":true`) {
		t.Fatalf("question state lost: %s", w.Body.String())
	}
}
func TestPaperNote(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	w := request(t, s, "GET", "/api/documents/"+d.ID+"/note", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"body":""`) {
		t.Fatal(w.Body.String())
	}
	if w = request(t, s, "PUT", "/api/documents/"+d.ID+"/note", strings.NewReader(`{"body":"Main idea\nsecond line"}`)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = request(t, s, "GET", "/api/documents/"+d.ID+"/note", nil)
	if !strings.Contains(w.Body.String(), `Main idea\nsecond line`) {
		t.Fatal(w.Body.String())
	}
	if w = request(t, s, "PUT", "/api/documents/missing/note", strings.NewReader(`{"body":"x"}`)); w.Code != 404 {
		t.Fatalf("missing document: %d", w.Code)
	}
	if w = request(t, s, "PUT", "/api/documents/"+d.ID+"/note", strings.NewReader(`{"body":"`+strings.Repeat("长", 100001)+`"}`)); w.Code != 400 {
		t.Fatalf("oversized note: %d", w.Code)
	}
	// Permanent deletion removes the note with the document.
	request(t, s, "DELETE", "/api/documents/"+d.ID, nil)
	var count int
	s.Store.DB.QueryRow("SELECT count(*) FROM document_notes").Scan(&count)
	if count != 0 {
		t.Fatal("note left behind")
	}
}
func TestAnnotationColorChange(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	w := request(t, s, "POST", "/api/documents/"+d.ID+"/annotations", strings.NewReader(`{"kind":"highlight","location":{"type":"pdf","page":1},"quote":"q","note":"keep","color":"#e6b94c"}`))
	var a Annotation
	_ = json.Unmarshal(w.Body.Bytes(), &a)
	path := "/api/documents/" + d.ID + "/annotations/" + a.ID
	if w = request(t, s, "PATCH", path, strings.NewReader(`{"color":"red"}`)); w.Code != 400 {
		t.Fatalf("bad color accepted: %d", w.Code)
	}
	w = request(t, s, "PATCH", path, strings.NewReader(`{"color":"#e8746b"}`))
	_ = json.Unmarshal(w.Body.Bytes(), &a)
	if w.Code != 200 || a.Color != "#e8746b" || a.Note != "keep" {
		t.Fatalf("color: %d %+v", w.Code, a)
	}
}

func TestThemeAvailableBeforeRecovery(t *testing.T) {
	s := testServer(t)
	_, err := s.Store.DB.Exec(`INSERT OR REPLACE INTO settings(key,value) VALUES('neoread','{"appearance":"dark"}')`)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	s.processingReady = ready
	if err := s.PrepareStartupSettings(); err != nil {
		t.Fatal(err)
	}
	// Hold the only DB connection as recovery would; startup theme uses snapshot.
	connection, err := s.Store.DB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := request(t, s, "GET", "/api/settings?startup=1", nil)
	connection.Close()
	if result.Code != 200 || !strings.Contains(result.Body.String(), "dark") {
		t.Fatal(result.Body.String())
	}
	if result := request(t, s, "GET", "/api/documents", nil); result.Code != 200 {
		t.Fatal(result.Code)
	}
	done := make(chan int, 1)
	go func() { done <- request(t, s, "PUT", "/api/preferences/library", strings.NewReader(`{}`)).Code }()
	select {
	case <-done:
		t.Fatal("library API raced recovery")
	case <-time.After(20 * time.Millisecond):
	}
	close(ready)
	select {
	case code := <-done:
		if code != 200 {
			t.Fatal(code)
		}
	case <-time.After(time.Second):
		t.Fatal("library remained blocked")
	}
}
func TestDesktopRuntimeRequiresAuthenticationAndValidStage(t *testing.T) {
	t.Setenv("PATH", os.Getenv("PATH"))
	previous := runtimeenv.Default
	runtimeenv.Default = runtimeenv.New(true)
	t.Cleanup(func() { runtimeenv.Default.Stop(); runtimeenv.Default = previous })
	s := testServer(t)
	r := httptest.NewRequest("POST", "http://127.0.0.1:17840/api/desktop-runtime/path", strings.NewReader(`{"PATH":"injected"}`))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	if result := request(t, s, "POST", "/api/desktop-runtime/path", strings.NewReader(`{"HOME":"injected"}`)); result.Code != 400 {
		t.Fatal(result.Code)
	}
	if result := request(t, s, "POST", "/api/desktop-runtime/path", strings.NewReader(`{"PATH":"safe-test-path"}`)); result.Code != 200 {
		t.Fatal(result.Code)
	}
	if os.Getenv("PATH") != "safe-test-path" {
		t.Fatal("PATH not applied")
	}
	if result := request(t, s, "POST", "/api/desktop-runtime/path", strings.NewReader(`{"PATH":"replacement"}`)); result.Code != 400 {
		t.Fatal("accepted duplicate", result.Code)
	}
}
func TestAsyncProcessingStopsBeforeStoreClosure(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	stop := s.StartProcessingAsync(ctx)
	cancel()
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("workers did not stop")
	}
	// idempotent stop is safe for error and shutdown paths.
	stop()
}

// Dispatch before testing parses CLI arguments. A copy/link of this native test
// executable acts as each provider on Windows as well as Unix, without a shell.
func TestMain(m *testing.M) {
	if os.Getenv("NEOREAD_AGENT_HELPER") != "" {
		switch os.Getenv("NEOREAD_AGENT_MODE") {
		case "web-events":
			runWebEventsProcess()
		case "translation-session":
			runTranslationSessionProcess()
		case "chat-session":
			runChatSessionProcess()
		case "document-conversation":
			runPersistentDocumentProcess()
		case "codex-handoff":
			runCodexHandoffProcess()
		case "document-reading":
			runDocumentReadingProcess()
		case "corpus-worker":
			runMissingPageCorpusProcess()
		case "models":
			runModelProcess()
		case "models-wait":
			time.Sleep(time.Minute)
		default:
			runAgentProcess()
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runModelProcess() {
	cwd, err := os.Getwd()
	if err != nil || filepath.Base(filepath.Dir(cwd)) != "ai-work" || !strings.HasPrefix(filepath.Base(cwd), "models-") {
		os.Exit(9)
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.Contains(line, `"subtype":"initialize"`):
			fmt.Println(`{"type":"control_response","response":{"request_id":"1","subtype":"success","response":{"models":[{"value":"default","resolvedModel":"model-a"},{"value":"alias","resolvedModel":"model-a","displayName":"Model A"}]}}}`)
		case strings.Contains(line, `"method":"initialize"`):
			fmt.Println(`{"id":1,"result":{}}`)
		case strings.Contains(line, `"method":"initialized"`):
		case strings.Contains(line, `"method":"model/list"`):
			fmt.Println(`{"id":2,"result":{"data":[{"model":"model-a","displayName":"Model A","isDefault":true}]}}`)
		case strings.Contains(line, `"method":"session/new"`):
			fmt.Println(`{"id":2,"result":{"configOptions":[{"id":"model","currentValue":"model-a","options":[{"value":"model-a","name":"Model A"}]}]}}`)
		default:
			os.Exit(8)
		}
	}
}
