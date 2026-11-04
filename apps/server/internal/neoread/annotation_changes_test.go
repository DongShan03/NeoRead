package neoread

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUnderlineMergeAPI(t *testing.T) {
	s := testServer(t)
	uploaded := upload(t, s, "reading-notes.pdf", sample(t, "reading-notes.pdf"))
	var doc Document
	if err := json.Unmarshal(uploaded.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	path := "/api/documents/" + doc.ID + "/annotations"
	save := func(x, width float64, quote string) Annotation {
		body, _ := json.Marshal(map[string]any{"kind": "underline", "quote": quote, "color": "#e6b94c", "location": map[string]any{"type": "pdf", "page": 1, "rects": []map[string]float64{{"x": x, "y": 0.2, "width": width, "height": 0.02}}}})
		w := request(t, s, "POST", path, bytes.NewReader(body))
		if w.Code != 201 {
			t.Fatalf("save: %d %s", w.Code, w.Body.String())
		}
		var a Annotation
		if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	first := save(0.1, 0.3, "reading, describe")
	duplicate := save(0.1, 0.3, "reading, describe")
	if first.ID != duplicate.ID {
		t.Fatalf("duplicate underline created a new record: %s != %s", first.ID, duplicate.ID)
	}
	save(0.12, 0.04, "rea")
	merged := save(0.3, 0.3, "describe the question")
	var items []Annotation
	w := request(t, s, "GET", path, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("overlapping underlines remain: %d", len(items))
	}
	if merged.ID != first.ID || items[0].Quote != "reading, describe the question" {
		t.Fatalf("wrong merged record: %+v", items[0])
	}
	var location annotationPDFLocation
	json.Unmarshal(items[0].Location, &location)
	if len(location.Rects) != 1 || math.Abs(location.Rects[0].X-0.1) > 1e-8 || math.Abs(location.Rects[0].Width-0.5) > 1e-8 {
		t.Fatalf("wrong union: %+v", location.Rects)
	}
}

func TestHighlightMergeAPI(t *testing.T) {
	s := testServer(t)
	uploaded := upload(t, s, "reading-notes.pdf", sample(t, "reading-notes.pdf"))
	var doc Document
	if err := json.Unmarshal(uploaded.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	path := "/api/documents/" + doc.ID + "/annotations"
	save := func(x, width float64, quote string) Annotation {
		body, _ := json.Marshal(map[string]any{"kind": "highlight", "quote": quote, "color": "#e6b94c", "location": map[string]any{"type": "pdf", "page": 1, "rects": []annotationRect{{X: x, Y: 0.2, Width: width, Height: 0.02}}}})
		w := request(t, s, "POST", path, bytes.NewReader(body))
		if w.Code != 201 {
			t.Fatalf("save: %d %s", w.Code, w.Body.String())
		}
		var a Annotation
		if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	first := save(0.1, 0.3, "one two")
	duplicate := save(0.1, 0.3, "one two")
	if duplicate.ID != first.ID {
		t.Fatalf("duplicate highlight created a record: %s != %s", first.ID, duplicate.ID)
	}
	merged := save(0.3, 0.3, "two three")
	w := request(t, s, "GET", path, nil)
	var items []Annotation
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || merged.ID != first.ID || items[0].Quote != "one two three" {
		t.Fatalf("highlights not merged: %+v", items)
	}
}

func TestSourceLinkedHighlightMerge(t *testing.T) {
	a := pdfUnderline("a", []annotationRect{{0.1, 0.2, 0.3, 0.02}}, "one two")
	a.Kind = "highlight"
	b := pdfUnderline("b", []annotationRect{{0.3, 0.2, 0.3, 0.02}}, "two three")
	b.Kind = "highlight"
	link := func(a Annotation, part string) Annotation {
		var location map[string]json.RawMessage
		json.Unmarshal(a.Location, &location)
		location["sentenceLink"], _ = json.Marshal(map[string]any{"origin": "source", "parts": []map[string]string{{"blockId": part, "sourceHash": "hash", "source": part, "target": part}}})
		a.Location, _ = json.Marshal(location)
		return a
	}
	a, b = link(a, "one"), link(b, "two")
	merged, ok := mergeAnnotationMark(a, b)
	if !ok || merged.Quote != "one two three" {
		t.Fatalf("source highlights not merged: %+v %v", merged, ok)
	}
	var location struct {
		SentenceLink struct {
			Origin string            `json:"origin"`
			Parts  []json.RawMessage `json:"parts"`
		} `json:"sentenceLink"`
	}
	if err := json.Unmarshal(merged.Location, &location); err != nil || location.SentenceLink.Origin != "source" || len(location.SentenceLink.Parts) != 2 {
		t.Fatalf("source links lost: %+v %v", location, err)
	}
	duplicate, ok := mergeAnnotationMark(merged, link(b, "two"))
	if !ok {
		t.Fatal("repeated source highlight was not deduplicated")
	}
	json.Unmarshal(duplicate.Location, &location)
	if len(location.SentenceLink.Parts) != 2 {
		t.Fatalf("duplicate source link: %+v", location.SentenceLink)
	}
	s := testServer(t)
	var doc Document
	json.Unmarshal(upload(t, s, "reading-notes.pdf", sample(t, "reading-notes.pdf")).Body.Bytes(), &doc)
	a.DocumentID, b.DocumentID = doc.ID, doc.ID
	if _, err := s.Store.saveAnnotation(a); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Store.saveAnnotation(b)
	if err != nil || saved.ID != a.ID {
		t.Fatalf("linked highlights saved separately: %+v %v", saved, err)
	}
	json.Unmarshal(saved.Location, &location)
	if len(location.SentenceLink.Parts) != 2 {
		t.Fatalf("saved source links lost: %+v", location.SentenceLink)
	}
	plain := pdfUnderline("plain", []annotationRect{{0.3, 0.2, 0.3, 0.02}}, "two three")
	plain.Kind = "highlight"
	if _, ok := mergeAnnotationMark(a, plain); ok {
		t.Fatal("merged linked and unlinked highlights")
	}
}

func TestEPUBHighlightMerge(t *testing.T) {
	a := epubUnderline("a", 0, 7, "one two")
	b := epubUnderline("b", 4, 13, "two three")
	a.Kind, b.Kind = "highlight", "highlight"
	merged, ok := mergeAnnotationMark(a, b)
	if !ok || merged.Quote != "one two three" {
		t.Fatalf("EPUB highlights not merged: %+v %v", merged, ok)
	}
	var location annotationEPUBLocation
	json.Unmarshal(merged.Location, &location)
	var locator annotationLocator
	json.Unmarshal([]byte(location.Locator), &locator)
	if locator.Locations.TextRange.Start != 0 || locator.Locations.TextRange.End != 13 {
		t.Fatalf("wrong EPUB highlight range: %+v", locator)
	}
	duplicate, ok := mergeAnnotationMark(merged, merged)
	if !ok || duplicate.Quote != merged.Quote {
		t.Fatal("repeated EPUB highlight was not deduplicated")
	}
}

func TestHighlightRestartRepair(t *testing.T) {
	s := testServer(t)
	var doc Document
	json.Unmarshal(upload(t, s, "reading-notes.pdf", sample(t, "reading-notes.pdf")).Body.Bytes(), &doc)
	for _, id := range []string{"first", "duplicate"} {
		a := pdfUnderline(id, []annotationRect{{0.1, 0.2, 0.3, 0.02}}, "same sentence")
		a.Kind, a.DocumentID = "highlight", doc.ID
		body, _ := json.Marshal(a)
		if _, err := s.Store.DB.Exec("INSERT INTO annotations VALUES(?,?,?)", id, doc.ID, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	root := s.Store.Root
	s.Store.DB.Close()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	var count int
	store.DB.QueryRow("SELECT COUNT(*) FROM annotations WHERE document_id=?", doc.ID).Scan(&count)
	if count != 1 {
		t.Fatalf("historical highlight duplicates remain: %d", count)
	}
}

func pdfUnderline(id string, rects []annotationRect, quote string) Annotation {
	location, _ := json.Marshal(annotationPDFLocation{Type: "pdf", Page: 1, Rects: rects})
	return Annotation{ID: id, DocumentID: "doc", Kind: "underline", Location: location, Quote: quote, Color: "#e6b94c", CreatedAt: now()}
}
func TestUnderlineMergeBoundaries(t *testing.T) {
	a := pdfUnderline("a", []annotationRect{{0.1, 0.2, 0.3, 0.02}}, "阅读原文")
	for _, kind := range []string{"note", "highlight", "bookmark"} {
		b := a
		b.Kind = kind
		if _, ok := mergeAnnotationMark(a, b); ok {
			t.Fatalf("merged %s into underline", kind)
		}
	}
	separate := []Annotation{
		pdfUnderline("disjoint", []annotationRect{{0.6, 0.2, 0.1, 0.02}}, "原文"),
		pdfUnderline("touching", []annotationRect{{0.4, 0.2, 0.1, 0.02}}, "原文"),
		pdfUnderline("other-line", []annotationRect{{0.1, 0.3, 0.3, 0.02}}, "阅读原文"),
	}
	otherPage := a
	otherPage.Location = json.RawMessage(`{"type":"pdf","page":2,"rects":[{"x":0.1,"y":0.2,"width":0.3,"height":0.02}]}`)
	otherDoc := a
	otherDoc.DocumentID = "other"
	separate = append(separate, otherPage, otherDoc)
	for _, b := range separate {
		if _, ok := mergeAnnotationMark(a, b); ok {
			t.Fatalf("merged separate ranges: %+v", b)
		}
	}
	b := pdfUnderline("b", []annotationRect{{0.3, 0.2001, 0.3, 0.02}}, "原文内容")
	merged, ok := mergeAnnotationMark(a, b)
	if !ok || merged.Quote != "阅读原文内容" {
		t.Fatalf("unicode overlap: %+v %v", merged, ok)
	}
}
func TestUnderlineMergeAcrossLines(t *testing.T) {
	a := pdfUnderline("a", []annotationRect{{0.1, 0.2, 0.6, 0.02}, {0.1, 0.3, 0.2, 0.02}}, "Before reading\nUse the outline")
	b := pdfUnderline("b", []annotationRect{{0.2, 0.3, 0.5, 0.02}, {0.1, 0.4, 0.4, 0.02}}, "outline to inspect\nSelect a sentence")
	merged, ok := mergeAnnotationMark(a, b)
	if !ok {
		t.Fatal("cross-line overlap not merged")
	}
	var location annotationPDFLocation
	json.Unmarshal(merged.Location, &location)
	if len(location.Rects) != 3 || math.Abs(location.Rects[1].Width-0.6) > 1e-8 {
		t.Fatalf("lines or coverage lost: %+v", location.Rects)
	}
	if merged.Quote != "Before reading Use the outline to inspect Select a sentence" {
		t.Fatalf("wrong quote: %s", merged.Quote)
	}
}

func TestUnderlineMergeRepeatedPhrase(t *testing.T) {
	a := pdfUnderline("a", []annotationRect{{0.1, 0.2, 0.2, 0.02}}, "word word")
	b := pdfUnderline("b", []annotationRect{{0.2, 0.2, 0.2, 0.02}}, "word word")
	merged, ok := mergeAnnotationMark(a, b)
	if !ok || merged.Quote != "word word word" {
		t.Fatalf("repeated words lost: %q", merged.Quote)
	}
}
func TestUnderlineBridgeAndRestartRepair(t *testing.T) {
	s := testServer(t)
	var doc Document
	json.Unmarshal(upload(t, s, "reading-notes.pdf", sample(t, "reading-notes.pdf")).Body.Bytes(), &doc)
	insert := func(a Annotation) {
		a.DocumentID = doc.ID
		body, _ := json.Marshal(a)
		if _, err := s.Store.DB.Exec("INSERT INTO annotations VALUES(?,?,?)", a.ID, doc.ID, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	left := pdfUnderline("left", []annotationRect{{0.1, 0.2, 0.2, 0.02}}, "one two")
	right := pdfUnderline("right", []annotationRect{{0.5, 0.2, 0.2, 0.02}}, "four five")
	bridge := pdfUnderline("bridge", []annotationRect{{0.2, 0.2, 0.4, 0.02}}, "two three four")
	left.Note = "keep this"
	right.Note = "and this"
	insert(left)
	insert(right)
	bridge.DocumentID = doc.ID
	result, err := s.Store.saveAnnotation(bridge)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "left" || result.Quote != "one two three four five" || len(result.ReplacedIDs) != 1 || result.ReplacedIDs[0] != "right" || result.Note != "keep this\n\nand this" {
		t.Fatalf("wrong bridge result: %+v", result)
	}
	// Simulate historical duplicates written by the old insertion-only API.
	duplicate := result.Annotation
	duplicate.ID = "old-duplicate"
	insert(duplicate)
	note := left
	note.ID = "note"
	note.Kind = "note"
	note.Note = "Do not remove"
	insert(note)
	root := s.Store.Root
	s.Store.DB.Close()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	items, err := store.DB.Query("SELECT body FROM annotations ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	var records []Annotation
	for items.Next() {
		var body []byte
		items.Scan(&body)
		var a Annotation
		json.Unmarshal(body, &a)
		records = append(records, a)
	}
	items.Close()
	if len(records) != 2 || records[0].ID != "left" || records[1].Note != "Do not remove" {
		t.Fatalf("legacy repair damaged records: %+v", records)
	}
	if err := store.compactMarks(); err != nil {
		t.Fatal(err)
	}
	var count int
	store.DB.QueryRow("SELECT COUNT(*) FROM annotations").Scan(&count)
	if count != 2 {
		t.Fatalf("repair not idempotent: %d", count)
	}
}
func TestConcurrentUnderlineSaves(t *testing.T) {
	s := testServer(t)
	var doc Document
	json.Unmarshal(upload(t, s, "reading-notes.pdf", sample(t, "reading-notes.pdf")).Body.Bytes(), &doc)
	var wg sync.WaitGroup
	failures := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := pdfUnderline(id(), []annotationRect{{0.1, 0.2, 0.2, 0.02}}, "same text")
			a.DocumentID = doc.ID
			_, err := s.Store.saveAnnotation(a)
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	s.Store.DB.QueryRow("SELECT COUNT(*) FROM annotations WHERE document_id=?", doc.ID).Scan(&count)
	if count != 1 {
		t.Fatalf("concurrent duplicate records: %d", count)
	}
}
func epubUnderline(id string, start, end int, quote string) Annotation {
	selector := "html:nth-of-type(1) > body:nth-of-type(1) > p:nth-of-type(1)"
	locator, _ := json.Marshal(map[string]any{"href": "chapter.xhtml", "type": "application/xhtml+xml", "locations": map[string]any{"domRange": annotationDOMRange{annotationDOMPoint{selector, 0, start}, annotationDOMPoint{selector, 0, end}}, "textRange": annotationTextRange{start, end}}, "text": map[string]string{"highlight": quote}})
	location, _ := json.Marshal(annotationEPUBLocation{Type: "epub", Href: "chapter.xhtml", Locator: string(locator)})
	return Annotation{ID: id, DocumentID: "doc", Kind: "underline", Location: location, Quote: quote}
}
func TestEPUBUnderlineUnion(t *testing.T) {
	a := epubUnderline("a", 0, 7, "one two")
	b := epubUnderline("b", 4, 13, "two three")
	merged, ok := mergeAnnotationMark(a, b)
	if !ok || merged.Quote != "one two three" {
		t.Fatalf("EPUB merge: %+v %v", merged, ok)
	}
	var location annotationEPUBLocation
	json.Unmarshal(merged.Location, &location)
	var locator annotationLocator
	json.Unmarshal([]byte(location.Locator), &locator)
	if locator.Locations.DOMRange.Start.CharOffset != 0 || locator.Locations.DOMRange.End.CharOffset != 13 || locator.Locations.TextRange.End != 13 {
		t.Fatalf("wrong EPUB range: %+v", locator)
	}
	contained := epubUnderline("c", 4, 7, "two")
	merged, ok = mergeAnnotationMark(a, contained)
	if !ok || merged.Quote != a.Quote {
		t.Fatal("contained EPUB underline changed quote")
	}
	separate := epubUnderline("d", 7, 13, "three")
	if _, ok = mergeAnnotationMark(a, separate); ok {
		t.Fatal("merged touching EPUB ranges")
	}
	// Legacy exact DOM ranges can still merge without the newer text offsets.
	var raw map[string]any
	json.Unmarshal([]byte(location.Locator), &raw)
	delete(raw["locations"].(map[string]any), "textRange")
	encoded, _ := json.Marshal(raw)
	location.Locator = string(encoded)
	merged.Location, _ = json.Marshal(location)
	if _, ok = mergeAnnotationMark(merged, b); !ok {
		t.Fatal("legacy DOM range did not merge")
	}
}

func TestLinkedAndTranslatedUnderlinesKeepTheirAnchors(t *testing.T) {
	for _, field := range []string{"translation", "sentenceLink"} {
		a := pdfUnderline("a", []annotationRect{{0.1, 0.2, 0.2, 0.02}}, "first")
		b := pdfUnderline("b", []annotationRect{{0.1, 0.2, 0.2, 0.02}}, "second")
		var location map[string]json.RawMessage
		if err := json.Unmarshal(b.Location, &location); err != nil {
			t.Fatal(err)
		}
		location[field] = json.RawMessage(`{"blockId":"p1-b1"}`)
		b.Location, _ = json.Marshal(location)
		if _, ok := mergeAnnotationMark(a, b); ok {
			t.Fatalf("merged and lost %s anchor", field)
		}
		if _, ok := mergeAnnotationMark(b, a); ok {
			t.Fatalf("merged incompatible %s anchors", field)
		}
	}
}

func TestSourceLinkedMarkDocumentOrder(t *testing.T) {
	type part struct {
		BlockID       string `json:"blockId"`
		SourceHash    string `json:"sourceHash"`
		SentenceIndex int    `json:"sentenceIndex"`
		Source        string `json:"source"`
		Target        string `json:"target"`
	}
	// Block IDs are deliberately not in lexical document order.
	parts := []part{{"z-first", "hash", 0, "A", "甲"}, {"m-middle", "hash", 0, "B", "乙"}, {"a-last", "hash", 0, "C", "丙"}}
	for _, format := range []string{"pdf", "epub"} {
		for _, kind := range []string{"highlight", "underline"} {
			for _, tc := range []struct {
				name               string
				oldStart, oldEnd   int
				newStart, newEnd   int
				oldParts, newParts []int
			}{
				{"extend backwards", 2, 5, 0, 3, []int{1, 2}, []int{0, 1}},
				{"contain older selection", 2, 3, 0, 5, []int{1}, []int{0, 1, 2}},
				{"extend forwards", 0, 3, 2, 5, []int{0, 1}, []int{1, 2}},
				{"same start", 0, 3, 0, 5, []int{0, 1}, []int{0, 1, 2}},
				{"fill middle link", 0, 5, 2, 3, []int{0, 2}, []int{1}},
			} {
				t.Run(format+"/"+kind+"/"+tc.name, func(t *testing.T) {
					makeMark := func(id string, start, end int, indexes []int) Annotation {
						quote := "A B C"[start:end]
						a := pdfUnderline(id, []annotationRect{{.1 + float64(start)*.02, .2, float64(end-start) * .02, .02}}, quote)
						if format == "epub" {
							a = epubUnderline(id, start, end, quote)
						}
						a.Kind = kind
						var location map[string]json.RawMessage
						json.Unmarshal(a.Location, &location)
						selected := []part{}
						for _, index := range indexes {
							selected = append(selected, parts[index])
						}
						location["sentenceLink"], _ = json.Marshal(map[string]any{"origin": "source", "parts": selected})
						a.Location, _ = json.Marshal(location)
						return a
					}
					old := makeMark("old", tc.oldStart, tc.oldEnd, tc.oldParts)
					incoming := makeMark("new", tc.newStart, tc.newEnd, tc.newParts)
					merged, ok := mergeAnnotationMark(old, incoming)
					if !ok || merged.ID != old.ID || merged.Quote != "A B C" {
						t.Fatalf("merge: %+v, %v", merged, ok)
					}
					var location struct {
						SentenceLink struct{ Parts []part } `json:"sentenceLink"`
					}
					json.Unmarshal(merged.Location, &location)
					if !reflect.DeepEqual(location.SentenceLink.Parts, parts) {
						t.Fatalf("wrong source/translation order or first navigation block: %+v", location.SentenceLink.Parts)
					}
				})
			}
		}
	}
}

func TestSourceSentenceLinkOrderBoundaries(t *testing.T) {
	part := func(block string, index int) map[string]any {
		// Identical excerpts at different sentence indexes are different links.
		return map[string]any{"blockId": block, "sourceHash": "h", "sentenceIndex": index, "source": "Repeated.", "target": "重复。"}
	}
	mark := func(parts ...map[string]any) Annotation {
		location, _ := json.Marshal(map[string]any{"sentenceLink": map[string]any{"origin": "source", "parts": parts}})
		return Annotation{Location: location}
	}
	t.Run("sentence indexes disambiguate equal selection boundaries", func(t *testing.T) {
		linked, ok := mergeSourceSentenceLinks(mark(part("block", 10)), mark(part("block", 2)), "Repeated. Repeated.")
		if !ok {
			t.Fatal("could not order sentences in the same block")
		}
		var link struct {
			Parts []struct {
				Index int `json:"sentenceIndex"`
			} `json:"parts"`
		}
		json.Unmarshal(linked, &link)
		if len(link.Parts) != 2 || link.Parts[0].Index != 2 || link.Parts[1].Index != 10 {
			t.Fatalf("repeated excerpts lost or numeric order reversed: %s", linked)
		}
	})
	t.Run("shared sentences order partial excerpts", func(t *testing.T) {
		a, b, c := part("z-first", 0), part("middle", 0), part("a-last", 0)
		linked, ok := mergeSourceSentenceLinks(mark(b, c), mark(a, b), "peated.")
		if !ok {
			t.Fatal("could not join ordered snapshots at their shared sentence")
		}
		var link struct {
			Parts []map[string]any `json:"parts"`
		}
		json.Unmarshal(linked, &link)
		if len(link.Parts) != 3 || link.Parts[0]["blockId"] != "z-first" || link.Parts[2]["blockId"] != "a-last" {
			t.Fatalf("partial excerpt reordered: %s", linked)
		}
	})
	t.Run("extracted anchors order sparse links", func(t *testing.T) {
		a, b := part("z-first", 0), part("a-last", 0)
		a["anchor"], b["anchor"] = "First sentence.", "Last sentence."
		linked, ok := mergeSourceSentenceLinks(mark(b), mark(a), "First\nsen\u00adtence. Last sentence.")
		if !ok {
			t.Fatal("could not order links by extracted anchors")
		}
		var link struct {
			Parts []map[string]any `json:"parts"`
		}
		json.Unmarshal(linked, &link)
		if len(link.Parts) != 2 || link.Parts[0]["blockId"] != "z-first" {
			t.Fatalf("extracted anchors ignored: %s", linked)
		}
	})
	t.Run("ambiguous blocks stay separate", func(t *testing.T) {
		if _, ok := mergeSourceSentenceLinks(mark(part("a", 0)), mark(part("z", 0)), "Repeated. Repeated."); ok {
			t.Fatal("invented an order for unrelated block IDs at equal boundaries")
		}
	})
	t.Run("contradictory snapshots stay separate", func(t *testing.T) {
		a, b := part("a", 0), part("b", 0)
		if _, ok := mergeSourceSentenceLinks(mark(a, b), mark(b, a), "Repeated. Repeated."); ok {
			t.Fatal("merged contradictory sentence orders")
		}
	})
}

func TestMarkMetadataCompatibility(t *testing.T) {
	for _, kind := range []string{"highlight", "underline"} {
		for _, tc := range []struct {
			name   string
			change func(*Annotation)
			merge  bool
		}{
			{"different color", func(a *Annotation) { a.Color = "#5b9fe8" }, false},
			{"same color case", func(a *Annotation) { a.Color = "#E6B94C" }, true},
			{"different tags", func(a *Annotation) { a.Tags = []string{"question"} }, false},
			{"different tag order", func(a *Annotation) { a.Tags = []string{"important", "method"} }, true},
			{"linked answer", func(a *Annotation) { a.AnswerID = "answer" }, false},
			{"resolved state", func(a *Annotation) { a.Resolved = true }, false},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				a := pdfUnderline("first", []annotationRect{{.1, .2, .3, .02}}, "one two")
				a.Kind, a.Tags = kind, []string{"method", "important"}
				b := a
				b.ID = "second"
				tc.change(&b)
				_, ok := mergeAnnotationMark(a, b)
				if ok != tc.merge {
					t.Fatalf("merge = %v, want %v", ok, tc.merge)
				}
			})
		}
	}
}

func TestHighlightRestartPreservesClassification(t *testing.T) {
	s := testServer(t)
	var doc Document
	json.Unmarshal(upload(t, s, "reading-notes.pdf", sample(t, "reading-notes.pdf")).Body.Bytes(), &doc)
	expected := map[string]string{}
	for _, item := range []struct{ id, color, tag string }{
		{"first", "#e6b94c", "method"},
		{"different-color", "#5b9fe8", "method"},
		{"different-tag", "#e6b94c", "question"},
		{"duplicate", "#e6b94c", "method"},
	} {
		a := pdfUnderline(item.id, []annotationRect{{.1, .2, .3, .02}}, "same sentence")
		a.Kind, a.DocumentID, a.Color, a.Tags = "highlight", doc.ID, item.color, []string{item.tag}
		a.Note = "Keep the note"
		body, _ := json.Marshal(a)
		if _, err := s.Store.DB.Exec("INSERT INTO annotations VALUES(?,?,?)", a.ID, doc.ID, string(body)); err != nil {
			t.Fatal(err)
		}
		if item.id != "duplicate" {
			expected[item.id] = string(body)
		}
	}
	root := s.Store.Root
	s.Store.DB.Close()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	actual, err := annotationSnapshot(store.DB, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actual) != len(expected) {
		t.Fatalf("startup lost highlight classifications: %v", actual)
	}
	for id, body := range expected {
		var want, got Annotation
		json.Unmarshal([]byte(body), &want)
		json.Unmarshal([]byte(actual[id]), &got)
		if got.ID != id || got.Color != want.Color || !reflect.DeepEqual(got.Tags, want.Tags) || got.Note != want.Note || got.Quote != want.Quote {
			t.Fatalf("startup changed classification %s: %+v", id, got)
		}
	}
}

func TestEditAnnotationNote(t *testing.T) {
	s := testServer(t)
	var doc Document
	json.Unmarshal(upload(t, s, "reading-notes.pdf", sample(t, "reading-notes.pdf")).Body.Bytes(), &doc)
	a := pdfUnderline("underline", []annotationRect{{0.1, 0.2, 0.2, 0.02}}, "Original passage")
	a.DocumentID = doc.ID
	saved, err := s.Store.saveAnnotation(a)
	if err != nil {
		t.Fatal(err)
	}
	// The editor may have opened before another selection extended this range.
	extension := pdfUnderline("extension", []annotationRect{{0.2, 0.2, 0.2, 0.02}}, "passage extended")
	extension.DocumentID = doc.ID
	saved, err = s.Store.saveAnnotation(extension)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/documents/" + doc.ID + "/annotations/" + a.ID
	for _, note := range []string{"Initial note", "Edited note", ""} {
		body, _ := json.Marshal(map[string]string{"note": note})
		w := request(t, s, "PATCH", path, strings.NewReader(string(body)))
		if w.Code != 200 {
			t.Fatalf("edit: %d %s", w.Code, w.Body.String())
		}
		var updated Annotation
		json.Unmarshal(w.Body.Bytes(), &updated)
		if updated.Note != note || updated.ID != saved.ID || updated.Kind != saved.Kind || updated.Quote != saved.Quote || string(updated.Location) != string(saved.Location) || updated.CreatedAt != saved.CreatedAt {
			t.Fatalf("edit changed annotation identity: %+v", updated)
		}
	}
	var count int
	s.Store.DB.QueryRow("SELECT COUNT(*) FROM annotations").Scan(&count)
	if count != 1 {
		t.Fatalf("edit duplicated annotation: %d", count)
	}
	if w := request(t, s, "PATCH", path, strings.NewReader(`{}`)); w.Code != 400 {
		t.Fatalf("missing note: %d", w.Code)
	}
	if w := request(t, s, "PATCH", "/api/documents/other/annotations/"+a.ID, strings.NewReader(`{"note":"wrong document"}`)); w.Code != 404 {
		t.Fatalf("cross-document edit: %d", w.Code)
	}
	request(t, s, "DELETE", path, nil)
	if w := request(t, s, "PATCH", path, strings.NewReader(`{"note":"late edit"}`)); w.Code != 404 {
		t.Fatalf("deleted annotation revived: %d", w.Code)
	}
}

func TestTagAnnotations(t *testing.T) {
	s := testServer(t)
	var doc Document
	json.Unmarshal(upload(t, s, "reading-notes.pdf", sample(t, "reading-notes.pdf")).Body.Bytes(), &doc)
	a := pdfUnderline("tagged", []annotationRect{{0.1, 0.2, 0.2, 0.02}}, "Passage")
	a.DocumentID = doc.ID
	if _, err := s.Store.saveAnnotation(a); err != nil {
		t.Fatal(err)
	}
	path := "/api/documents/" + doc.ID + "/annotations/" + a.ID
	patch := func(body string) Annotation {
		t.Helper()
		w := request(t, s, "PATCH", path, strings.NewReader(body))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
		var updated Annotation
		json.Unmarshal(w.Body.Bytes(), &updated)
		return updated
	}
	if got := patch(`{"tags":[" method ","Method","proof"]}`); strings.Join(got.Tags, ",") != "method,proof" {
		t.Fatalf("tags: %v", got.Tags)
	}
	// Other edits keep the tags.
	if got := patch(`{"note":"n","color":"#5b9fe8"}`); strings.Join(got.Tags, ",") != "method,proof" || got.Note != "n" {
		t.Fatalf("kept: %+v", got)
	}
	if got := patch(`{"tags":[]}`); len(got.Tags) != 0 {
		t.Fatalf("cleared: %v", got.Tags)
	}
	for _, bad := range []string{`{"tags":"x"}`, `{"tags":["a","b","c","d","e","f","g","h","i","j","k"]}`, `{"tags":[""]}`} {
		if w := request(t, s, "PATCH", path, strings.NewReader(bad)); w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
	list := request(t, s, "GET", "/api/documents/"+doc.ID+"/annotations", nil)
	if !strings.Contains(list.Body.String(), `"note":"n"`) {
		t.Fatal(list.Body.String())
	}
}

func undoRequest(t *testing.T, s *Server, session, method, path string, value any) *httptest.ResponseRecorder {
	t.Helper()
	body := ""
	if value != nil {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body = string(data)
	}
	r := httptest.NewRequest(method, "http://127.0.0.1:17840"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+s.Token)
	r.Header.Set(annotationUndoHeader, session)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func undoMark(t *testing.T, s *Server, session string, a Annotation) Annotation {
	t.Helper()
	w := undoRequest(t, s, session, "POST", "/api/documents/doc/annotations", a)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var result Annotation
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	return result
}
func undoLast(t *testing.T, s *Server, session string) {
	t.Helper()
	w := undoRequest(t, s, session, "POST", "/api/documents/doc/annotations/undo", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"undone":true`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
func TestAnnotationUndoRestoresEditsAndDeletedIdentity(t *testing.T) {
	s, _ := processingFixture(t)
	a := pdfUnderline("", []annotationRect{{.1, .2, .3, .02}}, "原文")
	a.Kind = "note"
	a.Note = "原始笔记"
	a.Tags = []string{"标签"}
	saved := undoMark(t, s, "client", a)
	original, _ := annotationSnapshot(s.Store.DB, "doc")
	w := undoRequest(t, s, "client", "PATCH", "/api/documents/doc/annotations/"+saved.ID, map[string]any{"note": "新笔记", "color": "#5b9fe8", "tags": []string{"新标签"}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	edited, _ := annotationSnapshot(s.Store.DB, "doc")
	w = undoRequest(t, s, "client", "DELETE", "/api/documents/doc/annotations/"+saved.ID, nil)
	if w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	undoLast(t, s, "client")
	restored, _ := annotationSnapshot(s.Store.DB, "doc")
	if !reflect.DeepEqual(restored, edited) {
		t.Fatal("delete undo lost saved fields")
	}
	undoLast(t, s, "client")
	restored, _ = annotationSnapshot(s.Store.DB, "doc")
	if !reflect.DeepEqual(restored, original) {
		t.Fatal("edit undo lost original fields")
	}
	undoLast(t, s, "client")
	restored, _ = annotationSnapshot(s.Store.DB, "doc")
	if len(restored) != 0 {
		t.Fatal("create undo left a mark")
	}
	w = undoRequest(t, s, "client", "POST", "/api/documents/doc/annotations/undo", nil)
	if !strings.Contains(w.Body.String(), `"undone":false`) {
		t.Fatal(w.Body.String())
	}
}
func TestAnnotationUndoRestoresAllMergedUnderlines(t *testing.T) {
	for _, kind := range []string{"pdf", "epub"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := processingFixture(t)
			var a, b, bridge Annotation
			if kind == "epub" {
				_, _ = s.Store.DB.Exec("UPDATE documents SET type='epub' WHERE id='doc'")
				a, b, bridge = epubUnderline("a", 0, 5, "first"), epubUnderline("b", 10, 15, "third"), epubUnderline("", 3, 12, "middle")
			} else {
				a = pdfUnderline("a", []annotationRect{{.1, .2, .2, .02}}, "first")
				b = pdfUnderline("b", []annotationRect{{.5, .2, .2, .02}}, "third")
				bridge = pdfUnderline("", []annotationRect{{.2, .2, .4, .02}}, "middle")
			}
			if _, err := s.Store.saveAnnotation(a); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Store.saveAnnotation(b); err != nil {
				t.Fatal(err)
			}
			before, _ := annotationSnapshot(s.Store.DB, "doc")
			undoMark(t, s, "client", bridge)
			after, _ := annotationSnapshot(s.Store.DB, "doc")
			if len(after) != 1 {
				t.Fatalf("expected one merged mark: %v", after)
			}
			undoLast(t, s, "client")
			restored, _ := annotationSnapshot(s.Store.DB, "doc")
			if !reflect.DeepEqual(before, restored) {
				t.Fatal("merged ranges or original identities lost")
			}
		})
	}
}
func TestAnnotationUndoRestoresMergedHighlights(t *testing.T) {
	s, _ := processingFixture(t)
	a := pdfUnderline("first", []annotationRect{{.1, .2, .3, .02}}, "one two")
	a.Kind = "highlight"
	if _, err := s.Store.saveAnnotation(a); err != nil {
		t.Fatal(err)
	}
	before, _ := annotationSnapshot(s.Store.DB, "doc")
	b := pdfUnderline("", []annotationRect{{.3, .2, .3, .02}}, "two three")
	b.Kind = "highlight"
	undoMark(t, s, "client", b)
	after, _ := annotationSnapshot(s.Store.DB, "doc")
	if len(after) != 1 || reflect.DeepEqual(before, after) {
		t.Fatalf("highlight was not merged: %v", after)
	}
	undoLast(t, s, "client")
	restored, _ := annotationSnapshot(s.Store.DB, "doc")
	if !reflect.DeepEqual(before, restored) {
		t.Fatal("undo lost the original highlight")
	}
}
func TestAnnotationUndoPreservesOtherSessionsAndRejectsStaleChanges(t *testing.T) {
	s, _ := processingFixture(t)
	a := pdfUnderline("", []annotationRect{{.1, .2, .2, .02}}, "one")
	a.Kind = "highlight"
	first := undoMark(t, s, "one", a)
	b := pdfUnderline("", []annotationRect{{.5, .2, .2, .02}}, "two")
	b.Kind = "highlight"
	second := undoMark(t, s, "two", b)
	undoLast(t, s, "one")
	items, _ := annotationSnapshot(s.Store.DB, "doc")
	if len(items) != 1 || items[second.ID] == "" || items[first.ID] != "" {
		t.Fatal("other client's annotation changed")
	}
	w := undoRequest(t, s, "one", "PATCH", "/api/documents/doc/annotations/"+second.ID, map[string]any{"note": "other client's edit"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	before, _ := annotationSnapshot(s.Store.DB, "doc")
	w = undoRequest(t, s, "two", "POST", "/api/documents/doc/annotations/undo", nil)
	if w.Code != 409 {
		t.Fatal(w.Body.String())
	}
	after, _ := annotationSnapshot(s.Store.DB, "doc")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("stale undo overwrote edits")
	}
}
func TestAnnotationUndoIgnoresFailedWritesAndIsDocumentScoped(t *testing.T) {
	s, _ := processingFixture(t)
	a := pdfUnderline("", []annotationRect{{.1, .2, .2, .02}}, "one")
	a.Kind = "highlight"
	undoMark(t, s, "client", a)
	w := undoRequest(t, s, "client", "PATCH", "/api/documents/doc/annotations/missing", map[string]any{"note": "bad"})
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	w = undoRequest(t, s, "client", "POST", "/api/documents/other/annotations/undo", nil)
	if strings.Contains(w.Body.String(), `"undone":true`) {
		t.Fatal("wrong document history")
	}
	undoLast(t, s, "client")
	items, _ := annotationSnapshot(s.Store.DB, "doc")
	if len(items) != 0 {
		t.Fatal("failed write consumed undo")
	}
}

func TestAnnotationUndoSkipsNoOpFieldNormalization(t *testing.T) {
	s, _ := processingFixture(t)
	a := pdfUnderline("", []annotationRect{{.1, .2, .2, .02}}, "one")
	a.Kind = "highlight"
	saved := undoMark(t, s, "client", a)
	w := undoRequest(t, s, "client", "PATCH", "/api/documents/doc/annotations/"+saved.ID, map[string]any{"color": saved.Color, "note": saved.Note})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if len(s.annotationHistory) != 1 {
		t.Fatal("no-op edit consumed history")
	}
	undoLast(t, s, "client")
	items, _ := annotationSnapshot(s.Store.DB, "doc")
	if len(items) != 0 {
		t.Fatal("normalized defaults prevented undo")
	}
}

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
