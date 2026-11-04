package neoread

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
