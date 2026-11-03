package neoread

import (
	"encoding/json"
	"strings"
	"testing"
)

func storedAnnotationBody(t *testing.T, s *Server, id string) string {
	t.Helper()
	var body string
	if err := s.Store.DB.QueryRow("SELECT body FROM annotations WHERE id=?", id).Scan(&body); err != nil {
		t.Fatal(err)
	}
	return body
}
func scopedAnnotationFixture(t *testing.T, s *Server, m layoutManifest) Annotation {
	t.Helper()
	parts := []translationMarkRange{}
	for _, b := range m.Blocks {
		parts = append(parts, translationMarkRange{BlockID: b.ID, SourceHash: translationHash(b.Text), SentenceIndexes: []int{0}, Start: 0, End: 3, Quote: "旧译文", SourceText: b.Text, TargetText: "旧译文", WholeSentence: true})
	}
	a := Annotation{ID: "multi", DocumentID: "doc", Kind: "note", Note: "keep note", Quote: "旧译文旧译文", CreatedAt: "2026-01-01"}
	a.Location = json.RawMessage(jsonText(map[string]any{"type": "pdf", "page": 1, "translation": translationMark{translationMarkRange: parts[0], Ranges: parts}}))
	if _, err := s.Store.DB.Exec("INSERT INTO annotations VALUES(?,?,?)", a.ID, a.DocumentID, jsonText(a)); err != nil {
		t.Fatal(err)
	}
	return a
}
func TestRetranslationRecoversOnlyCompletedParagraphRanges(t *testing.T) {
	s, p, m := completedTranslationFixture(t)
	a := scopedAnnotationFixture(t, s, m)
	unrelated := `{"id":"unrelated","location":{"type":"pdf","page":99},"note":{"not-decoded":true}}`
	if _, err := s.Store.DB.Exec("INSERT INTO annotations VALUES('unrelated','doc',?)", unrelated); err != nil {
		t.Fatal(err)
	}
	current := newTranslation(m.Blocks[0])
	current.Status = "complete"
	current.Sentences = []TranslationSentence{{Source: m.Blocks[0].Text, Target: "新的译文"}}
	if err := s.saveTranslation("doc", current, m); err != nil {
		t.Fatal(err)
	}
	var updated Annotation
	if err := json.Unmarshal([]byte(storedAnnotationBody(t, s, a.ID)), &updated); err != nil {
		t.Fatal(err)
	}
	before, after := recoveredMark(t, a), recoveredMark(t, updated)
	if after.Ranges[0].TargetText != "新的译文" || !after.Ranges[0].Recovered {
		t.Fatalf("not recovered: %+v", after)
	}
	if jsonText(before.Ranges[1]) != jsonText(after.Ranges[1]) {
		t.Fatal("untranslated range changed")
	}
	if updated.Note != a.Note || updated.CreatedAt != a.CreatedAt {
		t.Fatal("note metadata changed")
	}
	if storedAnnotationBody(t, s, "unrelated") != unrelated {
		t.Fatal("unrelated note changed")
	}
	if _, err := s.Store.DB.Exec("DELETE FROM annotations WHERE id='unrelated'"); err != nil {
		t.Fatal(err)
	}
	if err := s.queueRetranslation(p, m, translationRunOptions{}); err != nil {
		t.Fatal(err)
	}
	saved := storedAnnotationBody(t, s, a.ID)
	current.Status = "failed"
	if err := s.saveTranslation("doc", current, m); err != nil {
		t.Fatal(err)
	}
	if storedAnnotationBody(t, s, a.ID) != saved {
		t.Fatal("failed translation modified notes")
	}
}
func TestRetranslationMatchesReparsedBlockWithoutTouchingOtherRanges(t *testing.T) {
	s, p, m := completedTranslationFixture(t)
	a := scopedAnnotationFixture(t, s, m)
	if err := s.queueRetranslation(p, m, translationRunOptions{}); err != nil {
		t.Fatal(err)
	}
	m.Blocks[0].ID = "p1-b99"
	current := newTranslation(m.Blocks[0])
	current.Status = "complete"
	current.Sentences = []TranslationSentence{{Source: m.Blocks[0].Text, Target: "新译文"}}
	if err := s.saveTranslation("doc", current, m); err != nil {
		t.Fatal(err)
	}
	var updated Annotation
	_ = json.Unmarshal([]byte(storedAnnotationBody(t, s, a.ID)), &updated)
	mark := recoveredMark(t, updated)
	if mark.Ranges[0].BlockID != "p1-b99" || mark.Ranges[1].BlockID != m.Blocks[1].ID {
		t.Fatalf("incorrect scope: %+v", mark)
	}
}
func TestLegacyAnnotationMigrationIsExplicitAndIdempotent(t *testing.T) {
	s, _, m := completedTranslationFixture(t)
	b := m.Blocks[0]
	a := Annotation{ID: "legacy", DocumentID: "doc", Kind: "note", Note: "keep", Quote: "旧译文", CreatedAt: "2026-01-01"}
	a.Location = json.RawMessage(jsonText(map[string]any{"type": "pdf", "page": 1, "translation": translationMark{translationMarkRange: translationMarkRange{BlockID: b.ID, SourceHash: translationHash(b.Text), SentenceIndexes: []int{0}, Start: 0, End: 3}}, "sentenceLink": legacySentenceLink{Origin: "translation", Parts: []legacySentencePart{{BlockID: b.ID, SourceHash: translationHash(b.Text), SentenceIndex: 0, Source: b.Text, Target: "旧译文"}}}}))
	if _, err := s.Store.DB.Exec("INSERT INTO annotations VALUES(?,?,?)", a.ID, a.DocumentID, jsonText(a)); err != nil {
		t.Fatal(err)
	}
	source := Annotation{ID: "source", DocumentID: "doc", Kind: "note", Location: json.RawMessage(`{"type":"pdf","page":1}`), Note: "source note"}
	if _, err := s.Store.DB.Exec("INSERT INTO annotations VALUES(?,?,?)", source.ID, source.DocumentID, jsonText(source)); err != nil {
		t.Fatal(err)
	}
	_ = request(t, s, "GET", "/api/documents/doc/annotations", nil)
	if storedAnnotationBody(t, s, a.ID) != jsonText(a) {
		t.Fatal("reading notes migrated legacy data")
	}
	response := request(t, s, "POST", "/api/annotations/migrate-legacy", nil)
	var result annotationMigrationResult
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Migrated != 1 || result.Failed != 0 {
		t.Fatal(response.Body.String())
	}
	if strings.Contains(storedAnnotationBody(t, s, a.ID), "sentenceLink") {
		t.Fatal("legacy link remains")
	}
	if storedAnnotationBody(t, s, source.ID) != jsonText(source) {
		t.Fatal("modern source note changed")
	}
	response = request(t, s, "POST", "/api/annotations/migrate-legacy", nil)
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Migrated != 0 || result.Failed != 0 {
		t.Fatal("not idempotent", response.Body.String())
	}
	var count int
	_ = s.Store.DB.QueryRow("SELECT count(*) FROM annotations WHERE document_id='doc'").Scan(&count)
	if count != 3 {
		t.Fatal("duplicate or lost annotations", count)
	}
}

func TestScopedRecoveryAndTranslationCommitAtomically(t *testing.T) {
	s, _, m := completedTranslationFixture(t)
	a := scopedAnnotationFixture(t, s, m)
	before := storedAnnotationBody(t, s, a.ID)
	if _, err := s.Store.DB.Exec("CREATE TRIGGER reject_annotation BEFORE UPDATE ON annotations BEGIN SELECT RAISE(ABORT, 'test rollback'); END"); err != nil {
		t.Fatal(err)
	}
	current := newTranslation(m.Blocks[0])
	current.Status = "complete"
	current.Sentences = []TranslationSentence{{Source: m.Blocks[0].Text, Target: "新译文"}}
	if err := s.saveTranslation("doc", current, m); err == nil {
		t.Fatal("expected rollback")
	}
	if storedAnnotationBody(t, s, a.ID) != before {
		t.Fatal("annotation changed after rollback")
	}
	assertVisibleOldTranslation(t, s)
}
func TestManualMigrationPreservesUnmatchedLegacyAnnotations(t *testing.T) {
	s, _, _ := completedTranslationFixture(t)
	body := `{"id":"missing","documentId":"doc","kind":"note","note":"preserve","location":{"type":"pdf","page":50,"translation":{"blockId":"missing","sourceHash":"missing","sentenceIndexes":[0],"start":0,"end":2}}}`
	if _, err := s.Store.DB.Exec("INSERT INTO annotations VALUES('missing','doc',?)", body); err != nil {
		t.Fatal(err)
	}
	response := request(t, s, "POST", "/api/annotations/migrate-legacy", nil)
	var result annotationMigrationResult
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Migrated != 0 || result.Skipped != 1 {
		t.Fatal(response.Body.String())
	}
	if storedAnnotationBody(t, s, "missing") != body {
		t.Fatal("unmatched note was altered")
	}
}

func TestBilingualSentenceAnnotationKeepsOneIdentityThroughRecoveryAndRetranslation(t *testing.T) {
	s, p, m := completedTranslationFixture(t)
	a := scopedAnnotationFixture(t, s, m)
	mark := recoveredMark(t, a)
	for i := range mark.Ranges {
		mark.Ranges[i].Bilingual = true
	}
	mark.translationMarkRange = mark.Ranges[0]
	a.Location = json.RawMessage(jsonText(map[string]any{"type": "pdf", "page": 1, "translation": mark}))
	if _, err := s.Store.DB.Exec("UPDATE annotations SET body=? WHERE id=?", jsonText(a), a.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		out := request(t, s, "POST", "/api/documents/doc/annotations/recover", strings.NewReader(`{"force":true}`))
		if out.Code != 200 || !strings.Contains(out.Body.String(), `"checked":true`) {
			t.Fatal(out.Code, out.Body.String())
		}
	}
	if err := s.queueRetranslation(p, m, translationRunOptions{}); err != nil {
		t.Fatal(err)
	}
	m.Blocks[0].ID = "p1-b99"
	current := newTranslation(m.Blocks[0])
	current.Status = "complete"
	current.Sentences = []TranslationSentence{{Source: m.Blocks[0].Text, Target: "新的完整译文"}}
	if err := s.saveTranslation("doc", current, m); err != nil {
		t.Fatal(err)
	}
	var saved Annotation
	if err := json.Unmarshal([]byte(storedAnnotationBody(t, s, a.ID)), &saved); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM annotations WHERE document_id='doc'").Scan(&count); err != nil || count != 1 {
		t.Fatal("shared note was split", count, err)
	}
	recovered := recoveredMark(t, saved)
	if saved.ID != a.ID || saved.Note != a.Note || !recovered.Ranges[0].Bilingual || !recovered.Ranges[0].WholeSentence || recovered.Ranges[0].BlockID != "p1-b99" || recovered.Ranges[0].TargetText != "新的完整译文" {
		t.Fatal("shared identity or anchor lost", saved, recovered)
	}
}
