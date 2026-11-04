package neoread

import (
	"encoding/json"
	"strings"
	"testing"
)

func sentencesJSON(sources ...string) string {
	sentences := []TranslationSentence{}
	for _, source := range sources {
		sentences = append(sentences, TranslationSentence{Source: source, Target: "译文。"})
	}
	data, _ := json.Marshal(map[string]any{"sentences": sentences})
	return string(data)
}

func compact(s string) string { return strings.Join(strings.Fields(s), "") }

// Extracted from a VLDB paper: math italics with subscripts moved out of place.
const garbledParagraph = "Let 𝑇 be a set of transactions that depend on directly or tran- 𝑖 𝑡 𝑖 sitively, where for all 𝑡 in 𝑇 , there exists a path from 𝑖 𝑡 to in 𝑖 𝑡 𝑀𝑉 𝑆𝐺 ( 𝑠, ≪) through any types of edges, wr , rw , and ww . We refer to 𝑇 as the followers of 𝑡 𝑖."

func TestTranslationAcceptsRepairedExtractionAndAnchorsOriginal(t *testing.T) {
	model := []string{
		"Let T_i be a set of transactions that depend on t_i directly or transitively, where for all t in T_i, there exists a path from t to t_i in MVSG(s, ≪) through any types of edges, wr, rw, and ww.",
		"We refer to T_i as the followers of t_i.",
	}
	sentences, err := parseTranslation(sentencesJSON(model...), garbledParagraph)
	if err != nil {
		t.Fatal(err)
	}
	if sentences[0].Source != model[0] || sentences[1].Source != model[1] {
		t.Fatal("model sentences were not kept")
	}
	joined := sentences[0].Anchor + sentences[1].Anchor
	if compact(joined) != compact(garbledParagraph) {
		t.Fatalf("anchors do not cover the paragraph: %q", joined)
	}
	if !strings.HasPrefix(sentences[1].Anchor, "We refer to") {
		t.Fatalf("second anchor misplaced: %q", sentences[1].Anchor)
	}
}

func TestTranslationRepairsSplitWordsWithAnchors(t *testing.T) {
	source := "The A GENT reads the paper. It writes a sum- mary."
	sentences, err := parseTranslation(sentencesJSON("The AGENT reads the paper.", "It writes a summary."), source)
	if err != nil {
		t.Fatal(err)
	}
	if sentences[0].Anchor != "The A GENT reads the paper." || sentences[1].Anchor != "It writes a sum- mary." {
		t.Fatalf("anchors: %q %q", sentences[0].Anchor, sentences[1].Anchor)
	}
}

func TestExactTranslationNeedsNoAnchors(t *testing.T) {
	sentences, err := parseTranslation(sentencesJSON("First sentence.", "Second sentence."), "First sentence.  Second sentence.")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sentences {
		if s.Anchor != "" {
			t.Fatalf("exact sentence got an anchor: %+v", s)
		}
	}
}

func TestTranslationRejectsASentenceWithoutAnOriginalAnchor(t *testing.T) {
	source := "The attention mechanism connects the positions in the network and reduces the number of sequential operations needed to process the input."
	if _, err := parseTranslation(sentencesJSON(source, "XYZ XYZ XYZ."), source); err == nil {
		t.Fatal("accepted a translated sentence with no original source span")
	}
}

func TestCachedTranslationRepairsMissingAnchorsWithoutChangingSavedText(t *testing.T) {
	source := "The A GENT reads the paper. It writes a sum- mary."
	store := archiveFixture(t, t.TempDir())
	archiveDocument(t, store, "paper", "分类")
	s := NewServer(store, "test-token", "")
	t.Cleanup(s.Close)
	m := layoutManifest{Blocks: []PDFBlock{{ID: "p1-b1", Page: 1, Label: "text", Text: source}}}
	block := TranslationBlock{BlockID: "p1-b1", SourceHash: translationHash(source), Status: "complete", Sentences: []TranslationSentence{{Source: "The AGENT reads the paper.", Target: "第一句。"}, {Source: "It writes a summary.", Target: "第二句。"}}}
	if err := s.saveTranslation("paper", block); err != nil {
		t.Fatal(err)
	}
	got, err := s.translations("paper", m)
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	if got[0].Sentences[0].Anchor != "The A GENT reads the paper." || got[0].Sentences[1].Anchor != "It writes a sum- mary." {
		t.Fatal("legacy translation did not receive original sentence anchors", got)
	}
	if got[0].Sentences[0].Source != block.Sentences[0].Source || got[0].Sentences[1].Target != block.Sentences[1].Target {
		t.Fatal("saved prose changed", got)
	}
	if indexes := recoverySentenceIndexes(got[0], block.Sentences[1].Source); len(indexes) != 1 || indexes[0] != 1 {
		t.Fatal("existing annotations lost their sentence after restoring anchors", indexes)
	}
	var body string
	if err := store.DB.QueryRow("SELECT body FROM translations WHERE document_id='paper' AND block_id=? AND source_hash=?", block.BlockID, block.SourceHash).Scan(&body); err != nil || body != jsonText(block) {
		t.Fatal("reading rewrote the saved translation", body, err)
	}
}

func TestTranslationRejectsDroppedHalf(t *testing.T) {
	source := "Transactions commit in order. Readers never block writers in this design."
	if _, err := parseTranslation(sentencesJSON("Transactions commit in order."), source); err == nil {
		t.Fatal("accepted a reply missing most of the paragraph")
	}
}

func TestAnchorsFallBackProportionallyForHugeParagraphs(t *testing.T) {
	source := strings.Repeat("alpha beta gamma. ", 400)
	model := []string{strings.Repeat("alpha beta gamma. ", 200), strings.Repeat("alpha beta gamma. ", 200)}
	sentences := []TranslationSentence{{Source: model[0]}, {Source: model[1]}}
	anchors := alignAnchors(source+" extra", sentences)
	if compact(anchors[0]+anchors[1]) != compact(source+" extra") {
		t.Fatal("proportional anchors lost text")
	}
}
