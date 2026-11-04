package neoread

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWebDAVMovedAttachmentRemainsAvailableAfterOneSync(t *testing.T) {
	// Vary the map's iteration order across runs: the old document's removal
	// and the new document's file installation refer to the same physical blob.
	for attempt := range 8 {
		t.Run(string(rune('A'+attempt)), func(t *testing.T) {
			remote, _ := testDAV(t)
			a, b := syncFixture(t, remote, "Mac"), syncFixture(t, remote, "Windows")
			attachment := []byte("attachment moved to another paper")
			hash := syncBytesHash(attachment)
			add := func(owner string) {
				syncDocumentFixture(t, a.Store, owner, "分类")
				if _, err := a.Store.DB.Exec(`INSERT INTO paper_attachments(id,document_id,kind,title,sha256,created_at) VALUES(?,?,'file','shared',?,'now')`, owner, owner, hash); err != nil {
					t.Fatal(err)
				}
			}
			add("paper")
			if err := os.WriteFile(filepath.Join(a.Store.Root, "attachments", hash), attachment, 0600); err != nil {
				t.Fatal(err)
			}
			runSync(t, a)
			runSync(t, b)
			add("new-paper")
			syncAPI(t, a, "DELETE", "/api/documents/paper")
			runSync(t, a)
			runSync(t, b)
			if _, err := b.Store.Document("new-paper"); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(b.Store.Root, "attachments", hash))
			if err != nil || !bytes.Equal(body, attachment) {
				t.Fatal("attachment was lost while its new reference was installed", err)
			}
			runSync(t, b)
		})
	}
}
