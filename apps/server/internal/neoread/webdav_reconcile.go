package neoread

// Resolve one document using only its causal parents. A compact device index
// may expose another document before this one is fetched.
func resolveSyncVersions(state *syncLocalState, key string, versions []*syncVersion, name string) (*syncVersion, bool, *webdavConflict) {
	selected := versions[0]
	merged := false
	for _, entry := range versions[1:] {
		if sameSyncValue(selected, entry) {
			continue
		}
		if !selected.Deleted && !entry.Deleted {
			if body, ok := mergeSyncDocument(state.Base[key], selected.Body, entry.Body); ok {
				selected = &syncVersion{Body: body, Clock: syncClock{}}
				merged = true
				continue
			}
		}
		selected = nil
		break
	}
	conflictID := syncHash(versions)
	if selected == nil {
		choice := state.Choices[key]
		if choice.Conflict == conflictID {
			if choice.KeepBoth {
				if body, ok := keepSyncNotes(state.Base[key], versions); ok {
					selected = &syncVersion{Body: body}
					merged = true
				}
			}
			for _, version := range versions {
				if !choice.KeepBoth && syncHash(version) == choice.Version {
					selected = version
					// Resolve just the note when all other records can merge;
					// choosing its text must not discard independent annotations.
					if body, ok := keepSyncNotes(state.Base[key], versions); ok {
						for recordKey, record := range body.Records {
							if len(record["document_notes"]) == 1 {
								if note, exists := version.Body.Records[recordKey]; exists {
									body.Records[recordKey] = note
								} else {
									delete(body.Records, recordKey)
								}
							}
						}
						selected = &syncVersion{Body: body}
					}
					merged = true
					break
				}
			}
		}
		if selected == nil {
			conflict := syncConflict(key, conflictID, versions)
			_, conflict.CanKeepBoth = keepSyncNotes(state.Base[key], versions)
			return nil, false, &conflict
		}
	}
	if merged {
		body := selected.Body
		if selected.Deleted {
			body = nil
		}
		parents := syncClock{}
		for _, version := range versions {
			parents.include(version.Clock)
		}
		selected = newSyncVersion(state, body, selected.Body, name, parents)
	}
	return selected, merged, nil
}
