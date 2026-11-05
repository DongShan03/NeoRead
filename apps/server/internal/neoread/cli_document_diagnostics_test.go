package neoread

import (
	"encoding/json"
	"neoread.local/server/internal/diagnostics"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIDocumentDiagnosticsContainCountsAndStatesOnly(t *testing.T) {
	for _, tc := range []struct{ provider, mode, status string }{{"claude", "document-tools", "passed"}, {"claude", "document-extra", "extra_tools"}, {"claude", "document-missing", "missing_tools"}, {"kimi", "document-tools", "inventory_unavailable"}, {"kimi", "document-unsupported", "unsupported_http"}, {"kimi", "document-unexpected", "inventory_unavailable"}} {
		t.Run(tc.provider+"/"+tc.mode, func(t *testing.T) {
			s, a, _ := libraryRetrievalFixture(t, []string{retrievalAbstract, "PRIVATE_BODY agent attacks."}, []string{retrievalAbstract})
			a.chat.Question = "PRIVATE_QUESTION"
			fakeAgent(t, tc.provider, tc.mode)
			logs, err := diagnostics.New(t.TempDir(), "retrieval-test")
			if err != nil {
				t.Fatal(err)
			}
			s.SetDiagnostics(logs)
			_, _ = streamCLIDocumentChat(t.Context(), cliAdapter{s.Store.Root, tc.provider, "test"}, AIInput{Chat: a.chat}, nil)
			files, err := os.ReadDir(logs.Dir)
			if err != nil {
				t.Fatal(err)
			}
			startup, retrieval := false, false
			for _, file := range files {
				data, err := os.ReadFile(filepath.Join(logs.Dir, file.Name()))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), "PRIVATE_") || strings.Contains(string(data), "interpretability") || strings.Contains(string(data), "Abstract.") {
					t.Fatal("content leaked into diagnostics", string(data))
				}
				for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
					var event map[string]any
					if json.Unmarshal([]byte(line), &event) != nil {
						t.Fatal(line)
					}
					if event["event"] == "chat.document_tools" {
						startup = event["status"] == tc.status
					}
					if event["event"] == "chat.library_retrieval" {
						retrieval = true
						if event["totalPapers"] != float64(2) {
							t.Fatal(event)
						}
						for _, field := range []string{"queryGroups", "hitPassages", "readPassages", "coveredPapers"} {
							if _, ok := event[field].(float64); !ok {
								t.Fatal(event)
							}
						}
					}
				}
			}
			if !startup || !retrieval {
				t.Fatal("missing structured retrieval diagnostic", startup, retrieval)
			}
		})
	}
}
