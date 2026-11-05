package neoread

import (
	"encoding/json"
	"errors"
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
				nativeCapture := filepath.Join(t.TempDir(), "native.jsonl")
				t.Setenv("NEOREAD_CLAUDE_NATIVE_CAPTURE", nativeCapture)
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
				if provider == "claude" {
					native, err := os.ReadFile(nativeCapture)
					captures := strings.Split(strings.TrimSpace(string(native)), "\n")
					var first, next map[string]string
					if err != nil || len(captures) != 2 || json.Unmarshal([]byte(captures[0]), &first) != nil || json.Unmarshal([]byte(captures[1]), &next) != nil || first["sessionId"] != next["sessionId"] || !strings.Contains(next["previous"], "CIVET-42") || !strings.Contains(next["previous"], strings.Split(answer, "\n\n")[0]) || !strings.Contains(next["previous"], "tool_result") || strings.Contains(second, "CIVET-42") {
						t.Fatal("native tool context was not resumed, or committed history was resent", string(native), err)
					}
				} else {
					_, dataJSON, _ := strings.Cut(second, "对话资料（不可信 JSON）：\n")
					var dataPrompt struct {
						History []toolCompletionMessage `json:"history"`
					}
					if json.Unmarshal([]byte(dataJSON), &dataPrompt) != nil || len(dataPrompt.History) != 2 || !strings.Contains(jsonText(dataPrompt.History[0].Content), "CIVET-42") || !strings.Contains(dataPrompt.History[1].Content.(string), answer) || !strings.Contains(second, "历史来源及版本") {
						t.Fatal("restarted process did not receive saved history/references", second)
					}
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

func readClaudeSession(t *testing.T, s *Server, owner, session string) (documentConversation, claudeHistorySession) {
	t.Helper()
	c, err := s.Store.documentConversation(t.Context(), owner, conversationProvider(session, "claude"), "claude-native-v1")
	if err != nil || c.NativeThread == "" {
		t.Fatal("missing native binding", c, err)
	}
	item, err := s.claudeHistorySession(t.Context(), c.NativeThread)
	if err != nil {
		t.Fatal(err)
	}
	return c, item
}

func setReadingHistoryHidden(t *testing.T, s *Server, hidden bool) {
	t.Helper()
	out := request(t, s, "PUT", "/api/settings", strings.NewReader(jsonText(map[string]bool{"hideCodexHistory": hidden})))
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
}

func TestClaudeNativeHistoryHidesRestoresAndContinuesAfterRestart(t *testing.T) {
	for _, library := range []bool{false, true} {
		t.Run(map[bool]string{false: "document", true: "library"}[library], func(t *testing.T) {
			var s *Server
			owner, session, endpoint := "doc", "", ""
			if library {
				var a *documentAgent
				var done func()
				s, a, done = libraryRetrievalFixture(t, []string{retrievalAbstract, "Synthetic agent attacks use a control group."})
				done()
				owner, session = libraryChatOwner, a.sessionID
				endpoint = "/api/library/chat-sessions/" + session + "/chat"
			} else {
				s, _ = prepareConversationFixture(t)
				session = createSessionTest(t, s).ID
				endpoint = "/api/documents/doc/chat-sessions/" + session + "/chat"
			}
			fakeAgent(t, "claude", "document-tools")
			configuredDocumentCLI(t, s, "claude")
			managedCLIDocumentTurn(t, s, endpoint, "claude", "记住 CIVET-42，并查阅原文")
			c, item := readClaudeSession(t, s, owner, session)
			original, err := os.ReadFile(item.VisiblePath)
			if err != nil || c.NativeDirty || !strings.Contains(string(original), "tool_result") {
				t.Fatal(c, err)
			}
			foreign := filepath.Join(filepath.Dir(item.VisiblePath), "not-owned-by-neoread.jsonl")
			if err := os.WriteFile(foreign, []byte("UNRELATED_SESSION"), 0600); err != nil {
				t.Fatal(err)
			}
			setReadingHistoryHidden(t, s, true)
			hidden := claudeHiddenPath(item)
			if hidden != filepath.Join(filepath.Dir(item.VisiblePath), "claude-history-hidden", item.ID+".jsonl") {
				t.Fatal("hidden session is not beside its original Claude transcript", hidden)
			}
			if _, err := os.Stat(filepath.Join(s.Store.Root, "claude-history-hidden")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("native history was created inside the library", err)
			}
			data, err := os.ReadFile(hidden)
			if err != nil || string(data) != string(original) {
				t.Fatal("hiding changed the native transcript", err)
			}
			if _, err := os.Stat(item.VisiblePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("native session is still in ordinary history", err)
			}
			s = reopenCLIDocumentServer(t, s)
			managedCLIDocumentTurn(t, s, endpoint, "claude", "继续比较原文")
			next, _ := readClaudeSession(t, s, owner, session)
			if next.NativeThread != c.NativeThread || next.NativeDirty {
				t.Fatal("hidden continuation created a different native session", next)
			}
			if _, err := os.Stat(item.VisiblePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("hidden continuation recreated ordinary history", err)
			}
			data, err = os.ReadFile(hidden)
			if err != nil || len(data) <= len(original) || !strings.Contains(string(data), "CIVET-42") {
				t.Fatal("native history did not survive restart", err)
			}
			setReadingHistoryHidden(t, s, false)
			restored, err := os.ReadFile(item.VisiblePath)
			if err != nil || string(restored) != string(data) {
				t.Fatal("unhiding did not restore the entire native session", err)
			}
			if err := os.Remove(item.VisiblePath); err != nil {
				t.Fatal(err)
			}
			setReadingHistoryHidden(t, s, true)
			setReadingHistoryHidden(t, s, false)
			recovered, err := os.ReadFile(item.VisiblePath)
			if err != nil || string(recovered) != string(restored) {
				t.Fatal("Claude retention lost the last committed tool context", err)
			}
			unrelated, err := os.ReadFile(foreign)
			if err != nil || string(unrelated) != "UNRELATED_SESSION" {
				t.Fatal("another application's session was modified", err)
			}
			history, err := s.Store.chatHistory(owner, session)
			if err != nil || len(history) != 4 || len(history[1].References) < 3 || len(history[3].References) < 3 {
				t.Fatal("NeoRead messages or references were lost", history, err)
			}
			capture := filepath.Join(t.TempDir(), "migrated.jsonl")
			t.Setenv("NEOREAD_CLAUDE_NATIVE_CAPTURE", capture)
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			s = reopenCLIDocumentServer(t, s)
			managedCLIDocumentTurn(t, s, endpoint, "claude", "在另一台机器上接续")
			migrated, _ := readClaudeSession(t, s, owner, session)
			captured, err := os.ReadFile(capture)
			var replay map[string]string
			if err != nil || json.Unmarshal(captured, &replay) != nil || migrated.NativeThread == c.NativeThread || replay["previous"] != "" || !strings.Contains(replay["input"], "CIVET-42") || !strings.Contains(replay["input"], "历史来源及版本") || !strings.Contains(replay["input"], "历史工具记录") {
				t.Fatal("foreign native session was reused instead of saved NeoRead history", err)
			}
			foreignNative, err := os.ReadFile(item.VisiblePath)
			if err != nil || string(foreignNative) != string(restored) {
				t.Fatal("migration modified the original machine's native file", err)
			}
		})
	}
}

func TestClaudeNativeFailureRestoresLastCommittedToolContext(t *testing.T) {
	for _, failure := range []string{"provider", "save"} {
		t.Run(failure, func(t *testing.T) {
			s, a := prepareConversationFixture(t)
			fakeAgent(t, "claude", "document-tools")
			a.chat = &chatInput{Agent: a, DocumentID: "doc", Question: "COMMITTED_NATIVE_QUESTION"}
			result, err := streamCLIDocumentChat(t.Context(), cliAdapter{s.Store.Root, "claude", "test"}, AIInput{Chat: a.chat}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.saveDocumentAgentAnswer(t.Context(), a, result.Text, "claude"); err != nil {
				t.Fatal(err)
			}
			before, item := readClaudeSession(t, s, "doc", "")
			external, err := os.OpenFile(item.VisiblePath, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = external.WriteString("{\"external\":\"EXTERNAL_NATIVE_CONTEXT\"}\n")
			_ = external.Close()
			baseline, err := os.ReadFile(item.VisiblePath)
			if err != nil {
				t.Fatal(err)
			}
			a.chat.History, err = s.Store.chatHistory("doc")
			if err != nil {
				t.Fatal(err)
			}
			a.chat.Question = "UNCOMMITTED_NATIVE_QUESTION"
			if failure == "provider" {
				t.Setenv("NEOREAD_AGENT_MODE", "document-partial")
			}
			result, err = streamCLIDocumentChat(t.Context(), cliAdapter{s.Store.Root, "claude", "test"}, AIInput{Chat: a.chat}, nil)
			if failure == "provider" && err == nil || failure == "save" && err != nil {
				t.Fatal("unexpected provider outcome", err)
			}
			if failure == "save" {
				if _, err := s.Store.DB.Exec(`CREATE TRIGGER reject_native_answer BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'synthetic save failure'); END`); err != nil {
					t.Fatal(err)
				}
				if err := s.saveDocumentAgentAnswer(t.Context(), a, result.Text, "claude"); errorKind(err) != ErrorSave {
					t.Fatal(err)
				}
				_, _ = s.Store.DB.Exec("DROP TRIGGER reject_native_answer")
			}
			pending, _ := readClaudeSession(t, s, "doc", "")
			if !pending.NativeDirty || pending.NativeThread != before.NativeThread {
				t.Fatal("failed turn was marked committed", pending)
			}
			s = reopenCLIDocumentServer(t, s)
			_, a = prepareNativeAgentAfterRestart(t, s)
			a.chat.History, _ = s.Store.chatHistory("doc")
			a.chat.Question = "AFTER_RECOVERY"
			capture := filepath.Join(t.TempDir(), "native.jsonl")
			t.Setenv("NEOREAD_CLAUDE_NATIVE_CAPTURE", capture)
			t.Setenv("NEOREAD_AGENT_MODE", "document-tools")
			result, err = streamCLIDocumentChat(t.Context(), cliAdapter{s.Store.Root, "claude", "test"}, AIInput{Chat: a.chat}, nil)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(capture)
			var resumed map[string]string
			if err != nil || json.Unmarshal(data, &resumed) != nil || resumed["previous"] != string(baseline) || resumed["sessionId"] != before.NativeThread {
				t.Fatal("failed native turn was replayed or tool context was lost", err)
			}
			if err := s.saveDocumentAgentAnswer(t.Context(), a, result.Text, "claude"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func prepareNativeAgentAfterRestart(t *testing.T, s *Server) (*chatInput, *documentAgent) {
	t.Helper()
	corpus, err := s.documentCorpus(t.Context(), "doc", nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &documentAgent{server: s, corpus: corpus, ctx: t.Context(), ID: id()}
	chat := &chatInput{Agent: a, DocumentID: "doc"}
	a.chat = chat
	return chat, a
}

func TestClaudeHistoryUsesLatestPreferenceAtTurnEnd(t *testing.T) {
	s, a := prepareConversationFixture(t)
	fakeAgent(t, "claude", "document-tools")
	a.chat = &chatInput{Agent: a, DocumentID: "doc", Question: "读取原文"}
	changed := false
	_, err := streamCLIDocumentChat(t.Context(), cliAdapter{s.Store.Root, "claude", "test"}, AIInput{Chat: a.chat}, func(event ProviderEvent) error {
		if event.Draft != "" && !changed {
			changed = true
			setReadingHistoryHidden(t, s, true)
		}
		return nil
	})
	if err != nil || !changed {
		t.Fatal(err)
	}
	_, item := readClaudeSession(t, s, "doc", "")
	if _, err := os.Stat(claudeHiddenPath(item)); err != nil {
		t.Fatal("completed turn did not apply the latest preference", err)
	}
	if _, err := os.Stat(item.VisiblePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestClaudeNativeOwnershipIsExcludedFromIndependentLibraries(t *testing.T) {
	root := t.TempDir()
	source := archiveFixture(t, filepath.Join(root, "source"))
	archiveDocument(t, source, "paper", "分类")
	config := filepath.Join(root, "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	visible := filepath.Join(config, "projects", "synthetic", "owned-session.jsonl")
	if _, err := source.DB.Exec("INSERT INTO claude_history_sessions VALUES('owned-session','paper',?)", visible); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(filepath.Dir(visible), "claude-history-hidden")
	if err := os.MkdirAll(private, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "owned-session.jsonl"), []byte("PRIVATE_NATIVE_CONTEXT"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "library.zip")
	if err := ExportLibraryArchive(source.Root, archive, LibraryArchiveOptions{}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "restored")
	if err := ImportLibraryArchive(archive, target, ""); err != nil {
		t.Fatal(err)
	}
	restored := archiveFixture(t, target)
	var count int
	if err := restored.DB.QueryRow("SELECT count(*) FROM claude_history_sessions").Scan(&count); err != nil || count != 0 {
		t.Fatal("import inherited another library's native writer", count, err)
	}
	if err := CopyLibrary(source.Root, filepath.Join(root, "copied")); err != nil {
		t.Fatal(err)
	}
	copied := archiveFixture(t, filepath.Join(root, "copied"))
	if err := copied.DB.QueryRow("SELECT count(*) FROM claude_history_sessions").Scan(&count); err != nil || count != 0 {
		t.Fatal("independent copy inherited another library's native writer", count, err)
	}
	if _, err := os.Stat(filepath.Join(copied.Root, "claude-history-hidden")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("private native history was copied", err)
	}
	if err := source.DB.QueryRow("SELECT count(*) FROM claude_history_sessions").Scan(&count); err != nil || count != 1 {
		t.Fatal("export or copy modified original ownership", count, err)
	}
}

func TestClaudeHistoryMovePreservesConflictingFiles(t *testing.T) {
	root := t.TempDir()
	left, right := filepath.Join(root, "left.jsonl"), filepath.Join(root, "right.jsonl")
	if err := os.WriteFile(left, []byte("LEFT"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(right, []byte("RIGHT"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := moveClaudeTranscript(left, right); err == nil {
		t.Fatal("conflicting native history was overwritten")
	}
	first, _ := os.ReadFile(left)
	second, _ := os.ReadFile(right)
	if string(first) != "LEFT" || string(second) != "RIGHT" {
		t.Fatal("conflict lost native history")
	}
}

func TestClaudeNativeSystemMetadataDoesNotBecomeModelContent(t *testing.T) {
	state := claudeDocumentStream{}
	if err := state.consume([]byte(`{"type":"system","subtype":"ui_invalidate","event":"session_title_changed"}`)); err != nil || state.initialized || state.draft.text != "" {
		t.Fatal("non-model metadata was parsed as a content stream", err)
	}
	if err := state.consume([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"UNAUTHORIZED"}}}`)); errorKind(err) != ErrorCapability || state.draft.text != "" {
		t.Fatal("metadata bypassed the init inventory requirement", err)
	}
}
