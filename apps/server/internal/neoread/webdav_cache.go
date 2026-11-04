package neoread

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func syncCacheHash(name string) bool {
	hash, err := hex.DecodeString(name)
	return err == nil && len(hash) == 32
}

func readSyncCacheJSON(root *os.Root, name string, target any) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("同步缓存记录不是普通文件")
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("同步缓存记录包含多余内容")
	}
	return nil
}

// The sync operation lock protects collection from in-flight snapshots/downloads.
// Collect references from every local sync identity and cached remote version;
// a paused account or unresolved conflict may still need an older file chunk.
// Only local object copies are removed. Cloud deletion history stays untouched.
func (s *Server) pruneSyncCache(ctx context.Context, config webdavConfig, current *syncLocalState) (count int, bytes int64, result error) {
	root, err := os.OpenRoot(s.Store.Root)
	if err != nil {
		return 0, 0, err
	}
	defer root.Close()
	directory := ".neoread-sync"
	if _, err = root.Stat(filepath.Join(directory, "apply.json")); err == nil {
		return 0, 0, nil // Recovery must finish before any cache is retired.
	} else if !os.IsNotExist(err) {
		return 0, 0, err
	}
	identities, err := fs.ReadDir(root.FS(), directory)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	live := map[string]struct{}{}
	currentIdentity := filepath.Base(filepath.Dir(s.syncStatePath(config)))
	keep := func(doc *syncDocument) {
		if doc == nil {
			return
		}
		for _, file := range doc.Files {
			for _, chunk := range file.Chunks {
				live[chunk.Hash] = struct{}{}
			}
		}
	}
	for _, identity := range identities {
		if !syncCacheHash(identity.Name()) {
			continue
		}
		if err = ctx.Err(); err != nil {
			return 0, 0, err
		}
		folder := filepath.Join(directory, identity.Name())
		state := current
		if identity.Name() != currentIdentity {
			state = &syncLocalState{}
			err = readSyncCacheJSON(root, filepath.Join(folder, "state.json"), state)
		} else {
			err = nil // Reuse the state just saved by this run rather than decoding it twice.
		}
		if err == nil {
			if state.Own == nil || state.Base == nil || state.Files == nil {
				return 0, 0, errors.New("同步缓存状态不完整")
			}
			for _, version := range state.Own {
				if version == nil || version.Body == nil {
					return 0, 0, errors.New("同步缓存版本不完整")
				}
				keep(version.Body)
			}
			for _, doc := range state.Base {
				keep(doc)
			}
			for _, fingerprint := range state.Files {
				for _, chunk := range fingerprint.File.Chunks {
					live[chunk.Hash] = struct{}{}
				}
			}
		} else if !os.IsNotExist(err) {
			return 0, 0, err
		}
		remotes, err := fs.ReadDir(root.FS(), filepath.Join(folder, "remotes"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, 0, err
		}
		for _, remote := range remotes {
			device := strings.TrimSuffix(remote.Name(), ".json")
			if !strings.HasSuffix(remote.Name(), ".json") || !syncID(device) {
				continue
			}
			if err = ctx.Err(); err != nil {
				return 0, 0, err
			}
			var manifest syncManifest
			if err = readSyncCacheJSON(root, filepath.Join(folder, "remotes", remote.Name()), &manifest); err != nil {
				return 0, 0, err
			}
			if err = validateSyncManifest(&manifest, device); err != nil {
				return 0, 0, err
			}
			for _, version := range manifest.Entries {
				keep(version.Body)
			}
		}
	}
	objects, err := root.Open(filepath.Join(directory, "objects"))
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	defer objects.Close()
	for {
		entries, readErr := objects.ReadDir(128)
		if readErr != nil && readErr != io.EOF {
			return count, bytes, readErr
		}
		for _, entry := range entries {
			if err = ctx.Err(); err != nil {
				return count, bytes, err
			}
			if _, needed := live[entry.Name()]; needed || !syncCacheHash(entry.Name()) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return count, bytes, err
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if err = root.Remove(filepath.Join(directory, "objects", entry.Name())); err != nil {
				return count, bytes, err
			}
			count++
			bytes += info.Size()
		}
		if readErr == io.EOF {
			break
		}
	}
	return count, bytes, nil
}
