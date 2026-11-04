package neoread

import (
	"bufio"
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

func openAnnotationStream(t *testing.T, s *Server, host string) (func() string, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	req, _ := http.NewRequestWithContext(ctx, "GET", host+"/api/documents/doc/annotations/stream", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		response.Body.Close()
		cancel()
		t.Fatal(response.Status)
	}
	scanner := bufio.NewScanner(response.Body)
	return func() string {
		t.Helper()
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "data: ") {
				var revision string
				if err := json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &revision); err != nil {
					t.Fatal(err)
				}
				return revision
			}
		}
		t.Fatal("revision stream ended", scanner.Err())
		return ""
	}, func() { cancel(); response.Body.Close() }
}
func TestAnnotationStreamUpdatesOutsideTranslationScopeAndRecoversAfterDisconnect(t *testing.T) {
	s, _, m := completedTranslationFixture(t)
	a := scopedAnnotationFixture(t, s, m)
	// A subscription must not need the parsed publication, nor create a translation subscriber.
	if err := os.Remove(filepath.Join(s.analysisDir("doc"), "manifest.json")); err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(s.Handler())
	defer host.Close()
	read, closeStream := openAnnotationStream(t, s, host.URL)
	defer closeStream()
	initial := read()
	if len(initial) != 32 || len(s.translationSubscribers) != 0 {
		t.Fatal("invalid or coupled subscription")
	}
	current := newTranslation(m.Blocks[1])
	current.Status = "complete"
	current.Sentences = []TranslationSentence{{Source: m.Blocks[1].Text, Target: "更新第二段"}}
	if err := s.saveTranslation("doc", current, m); err != nil {
		t.Fatal(err)
	}
	updated := read()
	if updated == initial {
		t.Fatal("missing committed change notification")
	}
	var note Annotation
	_ = json.Unmarshal([]byte(storedAnnotationBody(t, s, a.ID)), &note)
	if recoveredMark(t, note).Ranges[1].TargetText != "更新第二段" {
		t.Fatal("notification preceded persistence")
	}
	closeStream()
	read, closeAgain := openAnnotationStream(t, s, host.URL)
	if read() != updated {
		t.Fatal("unchanged reconnect changed revision")
	}
	closeAgain()
	current.Sentences[0].Target = "断线期间更新"
	if err := s.saveTranslation("doc", current, m); err != nil {
		t.Fatal(err)
	}
	read, closeFinal := openAnnotationStream(t, s, host.URL)
	defer closeFinal()
	if read() == updated {
		t.Fatal("missed changes while disconnected")
	}
}
func TestAnnotationRevisionDoesNotAdvanceAfterRolledBackRecovery(t *testing.T) {
	s, _, m := completedTranslationFixture(t)
	scopedAnnotationFixture(t, s, m)
	before := s.annotationRevision("doc")
	if _, err := s.Store.DB.Exec("CREATE TRIGGER reject_annotation BEFORE UPDATE ON annotations BEGIN SELECT RAISE(ABORT, 'test rollback'); END"); err != nil {
		t.Fatal(err)
	}
	current := newTranslation(m.Blocks[0])
	current.Status = "complete"
	current.Sentences = []TranslationSentence{{Source: m.Blocks[0].Text, Target: "新译文"}}
	if err := s.saveTranslation("doc", current, m); err == nil {
		t.Fatal("expected rollback")
	}
	if s.annotationRevision("doc") != before {
		t.Fatal("published an uncommitted revision")
	}
}

func TestAnnotationMutationsPublishOnlyCommittedChanges(t *testing.T) {
	for _, session := range []string{"", "reader-window"} {
		t.Run("session="+session, func(t *testing.T) {
			s, _ := processingFixture(t)
			changes := make(chan struct{}, 1)
			s.annotationSubscribers = map[string]map[chan struct{}]struct{}{"doc": {changes: {}}}
			revision := s.annotationRevision("doc")
			changed := func() {
				t.Helper()
				if next := s.annotationRevision("doc"); next == revision {
					t.Fatal("committed mutation did not advance the revision")
				} else {
					revision = next
				}
				select {
				case <-changes:
				default:
					t.Fatal("subscriber did not receive the committed change")
				}
			}
			unchanged := func() {
				t.Helper()
				if s.annotationRevision("doc") != revision || len(changes) != 0 {
					t.Fatal("published a failed or unchanged mutation")
				}
			}
			a := pdfUnderline("", []annotationRect{{.1, .2, .3, .02}}, "source")
			saved := undoMark(t, s, session, a)
			changed()
			path := "/api/documents/doc/annotations/" + saved.ID
			edit := map[string]any{"note": "saved note"}
			if w := undoRequest(t, s, session, "PATCH", path, edit); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			changed()
			if w := undoRequest(t, s, session, "PATCH", path, edit); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			unchanged()
			if w := undoRequest(t, s, session, "PATCH", path, map[string]any{"color": "invalid"}); w.Code != 400 {
				t.Fatal(w.Body.String())
			}
			unchanged()
			if w := undoRequest(t, s, session, "DELETE", path, nil); w.Code != 204 {
				t.Fatal(w.Body.String())
			}
			changed()
			if session == "" {
				if len(s.annotationHistory) != 0 {
					t.Fatal("client without an undo session gained history")
				}
				return
			}
			undoLast(t, s, session)
			changed()
			if body := storedAnnotationBody(t, s, saved.ID); !strings.Contains(body, "saved note") {
				t.Fatal("undo notification preceded restored persistence")
			}
		})
	}
}
