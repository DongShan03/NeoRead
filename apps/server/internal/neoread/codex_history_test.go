package neoread

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexHiddenHistoryContinuesTheSameThreadAcrossVisibilityChanges(t *testing.T) {
	s, a := prepareConversationFixture(t)
	fakeAgent(t, "codex", "document-conversation")
	capture := filepath.Join(t.TempDir(), "capture.jsonl")
	t.Setenv("NEOREAD_CONVERSATION_CAPTURE", capture)
	t.Setenv("NEOREAD_DOCUMENT_TEST_BLOCK", a.corpus.Passages[1].ID)
	question := func(prompt string) {
		t.Helper()
		out := conversationRequest(t, s, "codex", prompt)
		if !strings.Contains(out.Body.String(), "event: done") || !strings.Contains(out.Body.String(), "CIVET-42") {
			t.Fatal(out.Body.String())
		}
	}
	question("记住 CIVET-42")
	var raw string
	if err := s.Store.DB.QueryRow("SELECT body FROM document_conversations WHERE document_id='doc' AND provider='codex'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var before documentConversation
	if err := json.Unmarshal([]byte(raw), &before); err != nil {
		t.Fatal(err)
	}
	for index, hide := range []bool{true, false, true, false} {
		out := request(t, s, "PUT", "/api/settings", strings.NewReader(jsonText(map[string]bool{"hideCodexHistory": hide})))
		if out.Code != 200 {
			t.Fatal(out.Code, out.Body.String())
		}
		if index == 0 {
			s.Close()
			s = NewServer(s.Store, "test-secret", "")
			t.Cleanup(s.Close)
		}
		question("继续说明刚才记住的内容")
		if err := s.Store.DB.QueryRow("SELECT body FROM document_conversations WHERE document_id='doc' AND provider='codex'").Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var current documentConversation
		if err := json.Unmarshal([]byte(raw), &current); err != nil || current.NativeThread != before.NativeThread {
			t.Fatal("visibility replaced the native conversation", err, current.NativeThread)
		}
		var hidden bool
		if err := s.Store.DB.QueryRow("SELECT hidden FROM codex_history_threads WHERE thread_id=?", before.NativeThread).Scan(&hidden); err != nil || hidden != hide {
			t.Fatal("visibility not applied", hidden, hide, err)
		}
	}
	history, err := s.Store.chatHistory("doc")
	if err != nil || len(history) != 10 {
		t.Fatal("visibility lost messages", len(history), err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	trace := string(data)
	if strings.Count(trace, `"method":"thread/start"`) != 1 || !strings.Contains(trace, `"method":"thread/unarchive"`) || strings.Contains(trace, `"method":"thread/delete"`) {
		t.Fatal("native history was recreated or deleted", trace)
	}
}

func TestCodexHistoryOwnershipStaysOnTheOriginalMachine(t *testing.T) {
	root := t.TempDir()
	source := archiveFixture(t, filepath.Join(root, "source"))
	archiveDocument(t, source, "paper", "分类")
	if _, err := source.DB.Exec("INSERT INTO codex_history_threads VALUES('owned-thread','paper',1)"); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "library.zip")
	if err := ExportLibraryArchive(source.Root, archive, LibraryArchiveOptions{}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := source.DB.QueryRow("SELECT count(*) FROM codex_history_threads").Scan(&count); err != nil || count != 1 {
		t.Fatal("export changed original ownership", count, err)
	}
	target := filepath.Join(root, "restored")
	if err := ImportLibraryArchive(archive, target, ""); err != nil {
		t.Fatal(err)
	}
	restored := archiveFixture(t, target)
	if err := restored.DB.QueryRow("SELECT count(*) FROM codex_history_threads").Scan(&count); err != nil || count != 0 {
		t.Fatal("import could control another library's Codex threads", count, err)
	}
}

func TestCodexHistoryVisibilityRejectsNonBooleanValues(t *testing.T) {
	s := testServer(t)
	if out := request(t, s, "PUT", "/api/settings", strings.NewReader(`{"hideCodexHistory":"yes"}`)); out.Code != 400 {
		t.Fatal(out.Code, out.Body.String())
	}
}
