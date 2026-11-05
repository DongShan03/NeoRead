package neoread

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"neoread.local/server/internal/diagnostics"
)

func libraryPageImageFixture(t *testing.T) (*Server, *documentAgent, map[string]map[int][]byte) {
	t.Helper()
	s := testServer(t)
	pixels := map[string]map[int][]byte{}
	for n, documentID := range []string{"alpha", "beta", "excluded"} {
		archiveDocument(t, s.Store, documentID, "研究")
		// Identical revisions and page numbers must still resolve by document.
		corpus := documentCorpus{Revision: sourceKey("shared page corpus"), Coverage: corpusCoverage{Status: "complete", TotalUnits: 2, SearchableUnits: 1}, Passages: []documentPassage{{ID: "p1-b1", Text: "Original passage", Section: "page-1", Location: json.RawMessage(`{"type":"pdf","page":1}`), Precision: "block"}}}
		if _, err := s.Store.DB.Exec("UPDATE document_corpora SET revision=?,body=? WHERE document_id=?", corpus.Revision, jsonText(corpus), documentID); err != nil {
			t.Fatal(err)
		}
		pixels[documentID] = map[int][]byte{}
		for _, page := range []int{1, 2} {
			canvas := image.NewRGBA(image.Rect(0, 0, 2, 2))
			canvas.Set(0, 0, color.RGBA{R: uint8(n + 1), G: uint8(page), A: 255})
			var encoded bytes.Buffer
			if err := png.Encode(&encoded, canvas); err != nil {
				t.Fatal(err)
			}
			pixels[documentID][page] = encoded.Bytes()
			name, err := pageImageName(corpus.Revision, page)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.Store.Root, "cache", documentID, "page-images", name)
			if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	item := createLibrarySessionTest(t, s, "alpha", "beta")
	corpus, finish, err := s.libraryCorpus(t.Context(), item.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(finish)
	a := &documentAgent{server: s, corpus: corpus, ctx: t.Context(), sessionID: item.ID, ID: id(), provider: "claude", canViewPages: true, chat: &chatInput{SessionID: item.ID}}
	return s, a, pixels
}

func TestLibraryPDFPageMCPImagesAndCitations(t *testing.T) {
	s, a, pixels := libraryPageImageFixture(t)
	url, token, stop, err := serveDocumentMCP(a)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-claude", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: documentMCPHTTPClient(token), DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, documentID := range []string{"alpha", "beta"} {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "read_pdf_pages", Arguments: map[string]any{"documentId": documentID, "pages": []int{2, 1, 2}}})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError || len(result.Content) != 5 {
			t.Fatalf("%s page images were not delivered: %s", documentID, jsonText(result))
		}
		metadata := resultPageImages(json.RawMessage(result.Content[0].(*mcp.TextContent).Text))
		if len(metadata) != 2 || a.corpus.Library == nil {
			t.Fatal("page order or library scope lost", metadata)
		}
		for i, page := range []int{2, 1} {
			label, labeled := result.Content[1+2*i].(*mcp.TextContent)
			picture, pictured := result.Content[2+2*i].(*mcp.ImageContent)
			if metadata[i].DocumentID != documentID || metadata[i].Page != page || !labeled || !strings.Contains(label.Text, "["+metadata[i].SourceID+"]") || !pictured || picture.MIMEType != "image/png" || !bytes.Equal(picture.Data, pixels[documentID][page]) {
				t.Fatalf("wrong image or label for %s page %d", documentID, page)
			}
		}
	}
	if len(a.Evidence) != 4 {
		t.Fatal("missing page evidence", a.Evidence)
	}
	answer := "图像比较"
	for _, evidence := range a.Evidence {
		// The API and Pi paths use the same resolver after subsequent tool calls.
		data, err := a.pageImageData(*evidence.PageImage)
		if err != nil || !bytes.Equal(data, pixels[evidence.DocumentID][evidence.PageImage.Page]) {
			t.Fatal("page evidence lost its document", err)
		}
		answer += " [" + evidence.SourceID + "]"
	}
	if err = s.saveDocumentAgentAnswer(t.Context(), a, answer, "claude"); err != nil {
		t.Fatal(err)
	}
	history, err := s.Store.chatHistory(libraryChatOwner, a.sessionID)
	if err != nil || len(history) != 1 {
		t.Fatal(history, err)
	}
	for _, evidence := range a.Evidence {
		path := fmt.Sprintf("/api/library/chat-sessions/%s/messages/%s/sources/%s", a.sessionID, history[0].ID, evidence.SourceID)
		response := request(t, s, "GET", path, nil)
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"documentId":"`+evidence.DocumentID+`"`) {
			t.Fatal("page citation resolved to the wrong document", response.Code, response.Body.String())
		}
	}
}

func TestPDFPageImageScopeAndLegacyConversation(t *testing.T) {
	s, a := pageImageFixture(t, true)
	want, picture, err := s.cachedPageImage("doc", a.corpus.Revision, 1)
	if err != nil {
		t.Fatal(err)
	}
	picture.DocumentID = ""
	data, err := a.pageImageData(picture)
	if err != nil || !bytes.Equal(data, want) {
		t.Fatal("legacy single-document image is no longer readable", err)
	}
	picture.DocumentID = "other"
	if _, err = a.pageImageData(picture); errorKind(err) != ErrorProtocol {
		t.Fatal("single-document image escaped its scope", err)
	}
}

func TestLibraryPDFPageImageFailuresHaveSafeReasons(t *testing.T) {
	s, a, _ := libraryPageImageFixture(t)
	_, picture, err := s.cachedPageImage("alpha", a.corpus.Library.Sources["alpha"].Revision, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, reason string
		change       func(*documentPageImage)
	}{
		{"outside_scope", "document_out_of_scope", func(p *documentPageImage) { p.DocumentID = "excluded" }},
		{"missing_document", "document_out_of_scope", func(p *documentPageImage) { p.DocumentID = "" }},
		{"missing_cache", "cache_unavailable", func(p *documentPageImage) { p.Page = 3 }},
		{"changed_image", "content_hash_mismatch", func(p *documentPageImage) { p.Hash = "changed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs, err := diagnostics.New(t.TempDir(), "page-images-test")
			if err != nil {
				t.Fatal(err)
			}
			s.SetDiagnostics(logs)
			changed := picture
			tc.change(&changed)
			data, err := a.pageImageData(changed)
			if errorKind(err) != ErrorProtocol || len(data) != 0 {
				t.Fatal("invalid image was delivered", err)
			}
			files, err := os.ReadDir(logs.Dir)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, file := range files {
				data, err := os.ReadFile(filepath.Join(logs.Dir, file.Name()))
				if err != nil {
					t.Fatal(err)
				}
				for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
					var event map[string]any
					if err = json.Unmarshal([]byte(line), &event); err != nil {
						t.Fatal(err)
					}
					if event["event"] != "document.page_image.failed" {
						continue
					}
					found = true
					if event["reasonCode"] != tc.reason || event["attemptId"] != a.ID || event["provider"] != "claude" || event["errorCode"] != "protocol" || event["level"] != "error" {
						t.Fatal(event)
					}
					for key := range event {
						switch key {
						case "event", "level", "timestamp", "sessionId", "source", "attemptId", "requestId", "provider", "page", "errorCode", "reasonCode":
						default:
							t.Fatal("unexpected diagnostic field", key)
						}
					}
				}
			}
			if !found {
				t.Fatal("missing page image failure reason")
			}
		})
	}
}
