package neoread

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

func syncEmptyIfNil(body *syncDocument) *syncDocument {
	if body == nil {
		return emptySyncDocument()
	}
	return body
}

func syncReceiveMergeBase(expected, starting, knownAtStart *syncDocument) *syncDocument {
	base := copySyncDocument(syncEmptyIfNil(expected))
	if knownAtStart != nil {
		for key := range knownAtStart.Records {
			if starting == nil || starting.Records[key] == nil {
				delete(base.Records, key)
			}
		}
		for name := range knownAtStart.Files {
			if _, ok := starting.FileSafe(name); !ok {
				delete(base.Files, name)
			}
		}
	}
	if starting != nil {
		for key, record := range starting.Records {
			base.Records[key] = record
		}
		for name, file := range starting.Files {
			base.Files[name] = file
		}
	}
	return base
}

func (s *Server) stagedSyncFingerprints(ctx context.Context, run string, before, target *syncDocument) (map[string]syncFingerprint, error) {
	result := map[string]syncFingerprint{}
	if target == nil {
		return result, nil
	}
	for name, declared := range target.Files {
		if old, ok := before.FileSafe(name); ok && syncHash(old) == syncHash(declared) {
			continue
		}
		file, err := os.Open(filepath.Join(s.syncDirectory(), "apply", run, "staged", filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return nil, err
		}
		actual := syncFile{Size: info.Size(), Chunks: []syncChunk{}}
		buffer := make([]byte, syncChunkSize)
		for index := 0; ; index++ {
			if err = ctx.Err(); err != nil {
				file.Close()
				return nil, err
			}
			n, readErr := io.ReadFull(file, buffer)
			if n > 0 {
				body := buffer[:n]
				hash := syncBytesHash(body)
				if index >= len(declared.Chunks) || declared.Chunks[index].Hash != hash || declared.Chunks[index].Size != n {
					if err = atomicFile(s.syncBlob(hash), body); err != nil {
						file.Close()
						return nil, err
					}
				}
				actual.Chunks = append(actual.Chunks, syncChunk{Hash: hash, Size: n})
			}
			if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
				break
			}
			if readErr != nil {
				file.Close()
				return nil, readErr
			}
		}
		if err = file.Close(); err != nil {
			return nil, err
		}
		result[name] = syncFingerprint{Size: info.Size(), Modified: info.ModTime().UnixNano(), File: actual}
		target.Files[name] = actual
	}
	return result, nil
}

func (s *Server) applySyncTarget(ctx context.Context, dav *davClient, initial *syncSnapshot, key string, target *syncDocument, plan *syncReceiving) (bool, error) {
	before := initial.observed(key)
	run := id()
	defer func() {
		if _, err := os.Stat(filepath.Join(s.syncDirectory(), "apply.json")); os.IsNotExist(err) {
			_ = os.RemoveAll(filepath.Join(s.syncDirectory(), "apply", run))
		}
	}()
	if target != nil {
		if err := s.stageSyncDocument(ctx, dav, run, before, target); err != nil {
			return false, err
		}
	}
	fingerprints, err := s.stagedSyncFingerprints(ctx, run, before, target)
	if err != nil {
		return false, err
	}
	finish, err := s.waitSyncApply(ctx, run, true)
	if err != nil {
		return false, err
	}
	defer finish()
	initial.ReceivePlans = map[string]*syncReceiving{key: plan}
	updates := map[string]*syncDocument{key: target}
	accepted, err := s.installSync(ctx, run, initial, updates)
	if err != nil {
		return false, err
	}
	if err = s.recoverSyncInstall(); err != nil {
		return false, err
	}
	if !accepted[key] {
		return false, nil
	}
	installed := updates[key]
	if initial.Observed == nil {
		initial.Observed = maps.Clone(initial.Documents)
	}
	initial.Observed[key] = installed
	if before != nil {
		for name := range before.Files {
			if _, ok := installed.FileSafe(name); !ok {
				delete(initial.Files, name)
			}
		}
	}
	for name, fingerprint := range fingerprints {
		initial.Files[name] = fingerprint
	}
	if initial.Receiving == nil {
		initial.Receiving = map[string]*syncReceiving{}
	}
	if plan == nil {
		delete(initial.Receiving, key)
		if receipt := initial.Receipts[key]; receipt != nil && initial.State != nil {
			applySyncReceipt(initial.State, key, receipt)
		}
	} else {
		initial.Receiving[key] = plan
	}
	s.notifySyncChanges(accepted)
	finish()
	return true, nil
}

func (s *Server) setSyncDocumentPhase(key, phase string) {
	s.webdav.mu.Lock()
	s.webdav.status.DocumentID = strings.TrimPrefix(key, "document/")
	s.webdav.status.DocumentPhase = phase
	task := s.webdav.taskID
	s.webdav.mu.Unlock()
	s.diagnosticEvent(slog.LevelInfo, "sync.document_stage", "taskId", task, "documentId", strings.TrimPrefix(key, "document/"), "phase", phase)
}

func (s *Server) receiveSyncPDF(ctx context.Context, dav *davClient, config webdavConfig, initial *syncSnapshot, state *syncLocalState, key string, desired, source *syncDocument, sourceClock syncClock, phaseLimit int) (bool, error) {
	starting := copySyncDocument(initial.observed(key))
	var knownAtStart *syncDocument
	parents := syncClock{}
	parents.include(state.BaseClocks[key])
	if previous := initial.Receiving[key]; previous != nil {
		knownAtStart = previous.Baseline
		parents = previous.Parents
	}
	for phase := 0; phase <= phaseLimit; phase++ {
		phaseName := syncPartNames[phase]
		s.setSyncPhase("downloading")
		s.setSyncDocumentPhase(key, phaseName)
		current := initial.observed(key)
		var expected *syncDocument
		if previous := initial.Receiving[key]; previous != nil {
			expected = previous.Baseline
		}
		wanted := syncReceivePhase(current, desired, phase)
		base := syncReceiveMergeBase(expected, starting, knownAtStart)
		target, ok := mergeSyncDocument(base, syncEmptyIfNil(current), wanted)
		if !ok {
			return false, nil // Keep a new local note for normal conflict choice.
		}
		baseline := syncReceivePhase(expected, source, phase)
		plan := &syncReceiving{Connection: state.Connection, Remote: source, Baseline: baseline, Clock: sourceClock, Parents: parents}
		// Keep relations until every new original is present. Their final pass
		// runs after the document loop, so cross-document parents are available.
		deferredRelations := false
		if phase == 2 {
			for recordKey, record := range target.Records {
				for _, row := range record["document_relations"] {
					var count int
					if err := s.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM documents WHERE id IN (?,?)", row["a"], row["b"]).Scan(&count); err != nil {
						return false, err
					}
					if count == 2 {
						continue
					}
					delete(target.Records, recordKey)
					delete(baseline.Records, recordKey)
					deferredRelations = true
				}
			}
			if !deferredRelations {
				plan = nil
			}
		}
		if syncHash(target) == syncHash(current) && initial.Receiving[key] != nil && plan != nil {
			continue
		}
		accepted, err := s.applySyncTarget(ctx, dav, initial, key, target, plan)
		if err != nil || !accepted {
			return false, err
		}
		if plan != nil {
			state.Base[key], state.BaseClocks[key] = baseline, parents
		}
		state.Files = initial.Files
		if err = s.saveSyncDocumentState(config, state, key); err != nil {
			return false, err
		}
		if phase == 2 {
			return !deferredRelations, nil
		}
	}
	return false, nil
}

func originalSyncVersion(versions []*syncVersion) *syncVersion {
	var chosen *syncVersion
	var original *syncDocument
	for _, version := range versions {
		if version.Deleted || !syncPDF(version.Body) {
			return nil
		}
		value := syncReceivePhase(nil, version.Body, 0)
		if original == nil {
			original, chosen = value, version
		} else {
			// Metadata may differ, but the original bytes must be identical.
			if syncHash(original.Files) != syncHash(value.Files) {
				return nil
			}
		}
	}
	return chosen
}

func receivingSource(initial *syncSnapshot, key string, versions []*syncVersion, device string, desired *syncDocument) (*syncDocument, syncClock) {
	var source *syncDocument
	clock := syncClock{}
	if previous := initial.Receiving[key]; previous != nil {
		source, clock = previous.Remote, maps.Clone(previous.Clock)
	}
	for _, version := range versions {
		if version.Device == device || version.Deleted {
			continue
		}
		if source == nil || version.Clock.covers(clock) {
			source, clock = version.Body, maps.Clone(version.Clock)
		}
	}
	if source == nil {
		source = desired
		for _, version := range versions {
			clock.include(version.Clock)
		}
	}
	return source, clock
}

type syncPDFReceiveFinish struct {
	Starting *syncDocument
	Known    *syncDocument
	Desired  *syncDocument
}

func (s *Server) finishReceivedPDF(ctx context.Context, dav *davClient, initial *syncSnapshot, key string, finish syncPDFReceiveFinish, desired map[string]*syncDocument, incoming map[string]bool) (bool, error) {
	previous := initial.Receiving[key]
	if previous == nil {
		return true, nil
	}
	base := syncReceiveMergeBase(previous.Baseline, finish.Starting, finish.Known)
	target, ok := mergeSyncDocument(base, syncEmptyIfNil(initial.observed(key)), finish.Desired)
	if !ok {
		return false, nil
	}
	for recordKey, record := range target.Records {
		for _, row := range record["document_relations"] {
			var count int
			if err := s.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM documents WHERE id IN (?,?)", row["a"], row["b"]).Scan(&count); err != nil {
				return false, err
			}
			if count == 2 {
				continue
			}
			for _, owner := range []string{fmtSyncString(row["a"]), fmtSyncString(row["b"])} {
				parent := "document/" + owner
				body, decided := desired[parent]
				if incoming[parent] && (!decided || body != nil) && initial.observed(parent) == nil {
					return false, nil
				}
			}
			delete(target.Records, recordKey)
		}
	}
	s.setSyncPhase("downloading")
	s.setSyncDocumentPhase(key, "other")
	return s.applySyncTarget(ctx, dav, initial, key, target, nil)
}
