package neoread

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func libraryRetrievalFixture(t *testing.T, papers ...[]string) (*Server, *documentAgent, func()) {
	t.Helper()
	s := testServer(t)
	ids := []string{}
	for n, texts := range papers {
		paperID := fmt.Sprintf("paper-%d", n)
		ids = append(ids, paperID)
		archiveDocument(t, s.Store, paperID, "研究")
		corpus := documentCorpus{Revision: "rev-" + paperID, Coverage: corpusCoverage{Status: "complete", TotalUnits: 2, SearchableUnits: 2}}
		if _, err := s.Store.DB.Exec("DELETE FROM document_passages_fts WHERE document_id=?", paperID); err != nil {
			t.Fatal(err)
		}
		for i, text := range texts {
			blockID := fmt.Sprintf("p1-b%d", i+1)
			corpus.Passages = append(corpus.Passages, documentPassage{ID: blockID, Text: text, Location: json.RawMessage(`{"type":"pdf","page":1}`), Section: "page-1", Precision: "block"})
			if _, err := s.Store.DB.Exec("INSERT INTO document_passages_fts VALUES(?,?,?,?)", paperID, corpus.Revision, blockID, text); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Store.DB.Exec("UPDATE document_corpora SET revision=?,body=? WHERE document_id=?", corpus.Revision, jsonText(corpus), paperID); err != nil {
			t.Fatal(err)
		}
	}
	item := createLibrarySessionTest(t, s, ids...)
	corpus, finish, err := s.libraryCorpus(t.Context(), item.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	done := func() { once.Do(finish) }
	t.Cleanup(done)
	chat := &chatInput{DocumentID: libraryChatOwner, SessionID: item.ID, Question: "研究方向建议"}
	a := &documentAgent{server: s, corpus: corpus, ctx: t.Context(), sessionID: item.ID, ID: id(), chat: chat}
	chat.Agent = a
	if err = a.prepareReading(t.Context(), AIInput{Chat: chat, Prompt: chat.Question}); err != nil {
		t.Fatal(err)
	}
	return s, a, done
}

const retrievalAbstract = "Abstract. This paper investigates language model security through controlled experiments and explains the limitations of its proposed approach."

func TestLibraryRetrievalFallback(t *testing.T) {
	s, a, _ := libraryRetrievalFixture(t, []string{"Title", retrievalAbstract, "Experiments demonstrate agent attacks."}, []string{"Title", "Abstract", retrievalAbstract})
	stats, err := a.preRetrieveLibrary(t.Context(), strings.Repeat("想研究这些论文的机制", 100)+"攻击agent流程")
	if err != nil || len(a.Evidence) != 2 || stats.Hits != 1 {
		t.Fatal(stats, err, a.Evidence)
	}
	if !strings.Contains(a.Evidence[0].Text, "agent") {
		t.Fatal(a.Evidence)
	}
	for _, trace := range a.Traces {
		if trace.Tool == "search_document" {
			var args struct{ Query string }
			if json.Unmarshal(trace.Arguments, &args) != nil || len(args.Query) > 512 || !utf8.ValidString(args.Query) {
				t.Fatal(trace)
			}
		}
	}
	answer := fmt.Sprintf("比较 [%s][%s]", a.Evidence[0].SourceID, a.Evidence[1].SourceID)
	if err = s.saveDocumentAgentAnswer(t.Context(), a, answer, "kimi"); err != nil {
		t.Fatal(err)
	}
}
func TestLibraryRetrievalBudgetAndEmptyEvidence(t *testing.T) {
	for _, budget := range []int{0, 250, 1000} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			_, a, _ := libraryRetrievalFixture(t, []string{retrievalAbstract}, []string{retrievalAbstract}, []string{strings.Repeat("large paragraph. ", 1000)})
			a.sourceTokens = budget
			stats, err := a.preRetrieveLibrary(t.Context(), "比较方向")
			if err != nil || a.evidenceTokens > budget {
				t.Fatal(err, a.evidenceTokens, budget)
			}
			if budget == 0 {
				text := a.libraryRetrievalContext(stats)
				if strings.Contains(text, "null") || !strings.Contains(text, "预算不足") || !strings.HasSuffix(text, "[]") {
					t.Fatal(text)
				}
			}
		})
	}
	_, a, _ := libraryRetrievalFixture(t, []string{"header", "tiny"})
	stats, err := a.preRetrieveLibrary(t.Context(), "比较方向")
	text := a.libraryRetrievalContext(stats)
	if err != nil || len(a.Evidence) != 0 || strings.Contains(text, "null") || !strings.Contains(text, "未找到符合") || jsonText(a.turnEvidence()) != "[]" {
		t.Fatal(text, err)
	}
	for _, raw := range []string{`{"error":"invalid_arguments"}`, `null`, `{`, `{}`} {
		if _, err := libraryToolItems(json.RawMessage(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	if items, err := libraryToolItems(json.RawMessage(`{"items":[]}`)); err != nil || len(items) != 0 {
		t.Fatal(err)
	}
}
