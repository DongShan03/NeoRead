package neoread

import "testing"

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
