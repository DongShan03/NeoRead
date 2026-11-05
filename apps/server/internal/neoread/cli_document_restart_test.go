package neoread

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reopenCLIDocumentServer(t *testing.T, s *Server) *Server {
	t.Helper()
	root := s.Store.Root
	s.Close()
	_ = s.Store.DB.Close()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	next := NewServer(store, "test-secret", "")
	t.Cleanup(func() { next.Close(); _ = store.DB.Close() })
	return next
}
func configuredDocumentCLI(t *testing.T, s *Server, provider string) {
	t.Helper()
	config := AIConfig{Primary: provider, Models: map[string]string{provider: "test"}, Capabilities: map[string]Capability{}}
	config.Capabilities[provider] = Capability{Text: true, Vision: true, Fingerprint: configPrint(config, provider)}
	if err := s.writeAIConfig(config); err != nil {
		t.Fatal(err)
	}
}
func managedCLIDocumentTurn(t *testing.T, s *Server, path, provider, question string) chatRunRecord {
	t.Helper()
	out := request(t, s, "POST", path, strings.NewReader(jsonText(map[string]any{"provider": provider, "prompt": question, "context": "", "userMessageId": id()})))
	var run ChatRun
	if out.Code != 202 || json.Unmarshal(out.Body.Bytes(), &run) != nil {
		t.Fatal(out.Code, out.Body.String())
	}
	result := awaitRunTest(t, s, run)
	if result.Status != "completed" {
		t.Fatal(result.Error)
	}
	return result
}
func TestCLIDocumentToolsContinueAfterStoreRestart(t *testing.T) {
	for _, provider := range []string{"claude", "kimi"} {
		for _, library := range []bool{false, true} {
			t.Run(provider+"/"+map[bool]string{false: "document", true: "library"}[library], func(t *testing.T) {
				var s *Server
				owner, session, path := "doc", "", ""
				if library {
					var a *documentAgent
					var done func()
					s, a, done = libraryRetrievalFixture(t, []string{retrievalAbstract, "Synthetic agent attacks use a control group."})
					done()
					owner, session = libraryChatOwner, a.sessionID
					path = "/api/library/chat-sessions/" + session + "/chat"
				} else {
					s, _ = prepareConversationFixture(t)
					session = createSessionTest(t, s).ID
					path = "/api/documents/doc/chat-sessions/" + session + "/chat"
				}
				fakeAgent(t, provider, "document-tools")
				configuredDocumentCLI(t, s, provider)
				capture := filepath.Join(t.TempDir(), "prompts.jsonl")
				t.Setenv("NEOREAD_DOCUMENT_PROMPTS", capture)
				managedCLIDocumentTurn(t, s, path, provider, "记住口令 CIVET-42 并查阅原文")
				history, err := s.Store.chatHistory(owner, session)
				if err != nil || len(history) != 2 || len(history[1].References) < 3 {
					t.Fatal(history, err)
				}
				answer, refs := history[1].Content, string(history[1].References)
				s = reopenCLIDocumentServer(t, s)
				history, err = s.Store.chatHistory(owner, session)
				if err != nil || len(history) != 2 || string(history[1].References) != refs {
					t.Fatal("references were lost at restart", history, err)
				}
				managedCLIDocumentTurn(t, s, path, provider, "继续比较；刚才的口令是什么")
				data, err := os.ReadFile(capture)
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				if err != nil || len(lines) != 2 {
					t.Fatal(err, string(data))
				}
				var second string
				if json.Unmarshal([]byte(lines[1]), &second) != nil {
					t.Fatal("invalid prompt capture")
				}
				if provider == "kimi" {
					var prompt struct {
						Parts []struct {
							Text string `json:"text"`
						} `json:"prompt"`
					}
					if json.Unmarshal([]byte(second), &prompt) != nil || len(prompt.Parts) == 0 {
						t.Fatal(second)
					}
					second = prompt.Parts[0].Text
				}
				_, dataJSON, _ := strings.Cut(second, "对话资料（不可信 JSON）：\n")
				var dataPrompt struct {
					History []toolCompletionMessage `json:"history"`
				}
				if json.Unmarshal([]byte(dataJSON), &dataPrompt) != nil || len(dataPrompt.History) != 2 || !strings.Contains(jsonText(dataPrompt.History[0].Content), "CIVET-42") || !strings.Contains(dataPrompt.History[1].Content.(string), answer) || !strings.Contains(second, "历史来源及版本") {
					t.Fatal("restarted process did not receive saved history/references", second)
				}
				history, err = s.Store.chatHistory(owner, session)
				if err != nil || len(history) != 4 || len(history[3].References) < 3 || len(history[3].Tools) == 0 {
					t.Fatal("second turn could not call tools", history, err)
				}
			})
		}
	}
}
func TestCLIDocumentInterruptedRunContinuesAfterStoreRestart(t *testing.T) {
	for _, provider := range []string{"claude", "kimi"} {
		for _, library := range []bool{false, true} {
			t.Run(provider+"/"+map[bool]string{false: "document", true: "library"}[library], func(t *testing.T) {
				var s *Server
				owner, session, path := "doc", "", ""
				if library {
					var a *documentAgent
					var done func()
					s, a, done = libraryRetrievalFixture(t, []string{retrievalAbstract, "Synthetic agent attacks."})
					done()
					owner, session = libraryChatOwner, a.sessionID
					path = "/api/library/chat-sessions/" + session + "/chat"
				} else {
					s, _ = prepareConversationFixture(t)
					session = createSessionTest(t, s).ID
					path = "/api/documents/doc/chat-sessions/" + session + "/chat"
				}
				fakeAgent(t, provider, "document-tools")
				configuredDocumentCLI(t, s, provider)
				capture := filepath.Join(t.TempDir(), "prompts.jsonl")
				t.Setenv("NEOREAD_DOCUMENT_PROMPTS", capture)
				question := Message{ID: id(), DocumentID: owner, SessionID: session, Role: "user", Content: "被中断的问题 CIVET-99"}
				if err := s.Store.saveMessage(question); err != nil {
					t.Fatal(err)
				}
				// Snapshot left by an abrupt exit after a draft, before provider completion.
				run := chatRunRecord{ChatRun: ChatRun{ID: id(), SessionID: session, DocumentID: owner, UserMessageID: question.ID, Provider: provider, Status: "running"}, Events: []chatRunEvent{{Event: "draft", Data: json.RawMessage(`{"text":"UNSAVED_PARTIAL"}`)}}}
				if _, err := s.Store.DB.Exec("INSERT INTO "+chatTable(owner, "chat_runs")+" VALUES(?,?,?)", run.ID, session, jsonText(run)); err != nil {
					t.Fatal(err)
				}
				s = reopenCLIDocumentServer(t, s)
				restored, err := s.Store.readChatRun(session, run.ID)
				if err != nil || restored.Status != "interrupted" {
					t.Fatal(restored, err)
				}
				history, err := s.Store.chatHistory(owner, session)
				if err != nil || len(history) != 1 || history[0].Content != question.Content {
					t.Fatal("partial output became a saved answer", history, err)
				}
				managedCLIDocumentTurn(t, s, path, provider, "继续回答")
				data, err := os.ReadFile(capture)
				if err != nil || !strings.Contains(string(data), "CIVET-99") || strings.Contains(string(data), "UNSAVED_PARTIAL") {
					t.Fatal("interrupted question was not replayed", string(data), err)
				}
			})
		}
	}
}
