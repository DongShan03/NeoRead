package neoread

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"neoread.local/server/internal/diagnostics"
)

// Exercise the process/protocol boundary with controlled events on every OS.
func init() {
	raw := os.Getenv("NEOREAD_PI_TEST_EVENTS")
	if raw == "" {
		return
	}
	var start struct {
		Input piRequest `json:"input"`
	}
	decoder := json.NewDecoder(os.Stdin)
	if decoder.Decode(&start) != nil {
		os.Exit(2)
	}
	var events []piEvent
	if json.Unmarshal([]byte(raw), &events) != nil {
		os.Exit(2)
	}
	exit := 0
	for _, event := range events {
		event.Version, event.RunID = 1, start.Input.RunID
		_ = json.NewEncoder(os.Stdout).Encode(event)
		if event.Type == "tool" {
			var result struct{ Type string }
			if decoder.Decode(&result) != nil || result.Type != "toolResult" {
				os.Exit(2)
			}
		}
		if event.Type == "error" {
			exit = 1
		}
	}
	fmt.Fprintln(os.Stderr, "PRIVATE_STDERR sk-private-test https://private.invalid")
	if os.Getenv("NEOREAD_PI_TEST_WAIT") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	os.Exit(exit)
}

func piFakeWorker(t *testing.T, s *Server, events ...piEvent) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEOREAD_NODE", executable)
	t.Setenv("NEOREAD_AGENT", "test-worker")
	t.Setenv("NEOREAD_PI_TEST_EVENTS", jsonText(events))
	if err := os.MkdirAll(filepath.Join(s.Store.Root, "ai-work"), 0700); err != nil {
		t.Fatal(err)
	}
}

func piDiagnosticRows(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(s.Diagnostics.Dir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
			var row map[string]any
			if err := json.Unmarshal(line, &row); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func TestPiWorkerFailureCodes(t *testing.T) {
	for _, tc := range []struct {
		code, text string
		kind       ErrorKind
	}{
		{"http_400", "请求参数", ErrorConfiguration},
		{"http_401", "API Key 无效", ErrorAuthentication},
		{"http_402", "余额或配额不足", ErrorBilling},
		{"http_403", "无权限", ErrorPermission},
		{"http_404", "模型或 API 接口不存在", ErrorModel},
		{"http_408", "超时", ErrorTimeout},
		{"http_409", "请求参数", ErrorConfiguration},
		{"http_422", "请求参数", ErrorConfiguration},
		{"http_429", "限流", ErrorLimit},
		{"http_5xx", "暂时不可用", ErrorUpstream},
		{"network", "无法连接", ErrorNetwork},
		{"timeout", "超时", ErrorTimeout},
		{"stream_interrupted", "连接中断", ErrorNetwork},
		{"stop_length", "输出上限", ErrorLimit},
		{"stop_other", "未正常结束", ErrorProtocol},
		{"empty_answer", "未返回回答", ErrorProtocol},
		{"output_limit", "输出上限", ErrorLimit},
		{"invalid_message", "消息格式无效", ErrorProtocol},
		{"PRIVATE_UNKNOWN_CODE", "未正常结束", ErrorProtocol},
	} {
		t.Run(tc.code, func(t *testing.T) {
			s := testServer(t)
			piFakeWorker(t, s, piEvent{Type: "error", Code: tc.code, Message: "PRIVATE_BODY sk-private-test"})
			result, err := s.runPiWorker(t.Context(), s.Store.Root, piRequest{RunID: "code-test"}, nil, nil)
			if err == nil || errorKind(err) != tc.kind || !strings.Contains(err.Error(), tc.text) || strings.Contains(err.Error(), "PRIVATE") || result.Type != "" {
				t.Fatalf("unexpected failure: %+v %v", result, err)
			}
		})
	}
}

func TestPiWorkerRetryActivityAndDiagnostics(t *testing.T) {
	s := testServer(t)
	logs, err := diagnostics.New(filepath.Join(s.Store.Root, "logs"), "pi-test")
	if err != nil {
		t.Fatal(err)
	}
	s.SetDiagnostics(logs)
	piFakeWorker(t, s,
		piEvent{Type: "retry", Attempt: 1, Max: 3, DelayMs: 1000, Status: 429, Attempts: 1},
		piEvent{Type: "draft", Text: "PRIVATE_DRAFT"},
		piEvent{Type: "error", Code: "stream_interrupted", Phase: "stream", Status: 200, Attempts: 2, Message: "PRIVATE_BODY sk-private-test"},
	)
	var events []piEvent
	ctx := context.WithValue(t.Context(), diagnosticRequestKey{}, "request-test")
	_, err = s.runPiWorker(ctx, s.Store.Root, piRequest{RunID: "retry-test"}, func(e piEvent) error { events = append(events, e); return nil }, nil)
	if errorKind(err) != ErrorNetwork {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != "activity" || events[0].Message != "模型服务繁忙，正在重试（1/3）" {
		t.Fatalf("unexpected events: %+v", events)
	}
	rows := piDiagnosticRows(t, s)
	if len(rows) != 3 {
		t.Fatal(rows)
	}
	retry, finish := rows[1], rows[2]
	for key, want := range map[string]any{"event": "pi.request.retry", "level": "warn", "attempt": float64(1), "maxAttempts": float64(3), "delayMs": float64(1000), "upstreamStatus": float64(429), "visibleOutput": false, "currentMessageVisible": false, "runId": "retry-test", "requestId": "request-test"} {
		if retry[key] != want {
			t.Errorf("retry %s = %v, want %v", key, retry[key], want)
		}
	}
	for key, want := range map[string]any{"event": "pi.run.finished", "level": "error", "status": "failed", "errorCode": "network", "reasonCode": "stream_interrupted", "phase": "stream", "visibleOutput": true, "currentMessageVisible": true, "attempts": float64(2), "upstreamStatus": float64(200)} {
		if finish[key] != want {
			t.Errorf("finish %s = %v, want %v", key, finish[key], want)
		}
	}
	if _, ok := finish["durationMs"].(float64); !ok {
		t.Fatal("missing duration", finish)
	}
	var bundle bytes.Buffer
	if err := logs.Export(&bundle, s.Store.Root, nil, nil); err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(bundle.Bytes()), int64(bundle.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range z.File {
		reader, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("PRIVATE")) || bytes.Contains(raw, []byte("sk-private-test")) || bytes.Contains(raw, []byte("private.invalid")) {
			t.Fatal("private content leaked", entry.Name)
		}
	}
}

func TestPiWorkerUnknownCodeAndHTTPDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		code, phase, want string
		status, attempts  int
	}{
		{"http_429", "request", "http_429", 429, 4},
		{"PRIVATE_CODE", "PRIVATE_PHASE", "unrecognized", 0, 1},
	} {
		t.Run(tc.code, func(t *testing.T) {
			s := testServer(t)
			logs, _ := diagnostics.New(filepath.Join(s.Store.Root, "logs"), "pi-test")
			s.SetDiagnostics(logs)
			piFakeWorker(t, s, piEvent{Type: "error", Code: tc.code, Phase: tc.phase, Status: tc.status, Attempts: tc.attempts})
			_, err := s.runPiWorker(t.Context(), s.Store.Root, piRequest{RunID: "failed"}, nil, nil)
			if err == nil {
				t.Fatal("failure lost")
			}
			rows := piDiagnosticRows(t, s)
			finish := rows[len(rows)-1]
			if finish["reasonCode"] != tc.want || finish["upstreamStatus"] != float64(tc.status) || finish["attempts"] != float64(tc.attempts) || finish["visibleOutput"] != false {
				t.Fatal(finish)
			}
			if strings.Contains(jsonText(rows), "PRIVATE") {
				t.Fatal("unknown values leaked")
			}
		})
	}
}

func TestPiWorkerRetrySuccessAndCancellation(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRun), func(t *testing.T) {
			s := testServer(t)
			logs, _ := diagnostics.New(filepath.Join(s.Store.Root, "logs"), "pi-test")
			s.SetDiagnostics(logs)
			retry := piEvent{Type: "retry", Attempt: 1, Max: 3, DelayMs: 20000, Status: 503, Attempts: 1}
			if cancelRun {
				piFakeWorker(t, s, retry)
				t.Setenv("NEOREAD_PI_TEST_WAIT", "1")
			} else {
				piFakeWorker(t, s, retry, piEvent{Type: "complete", Text: "ok", Entries: json.RawMessage(`[]`), Attempts: 2})
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			result, err := s.runPiWorker(ctx, s.Store.Root, piRequest{RunID: "settle"}, func(e piEvent) error {
				if cancelRun {
					cancel()
				}
				return nil
			}, nil)
			if cancelRun && !errors.Is(err, context.Canceled) || !cancelRun && (err != nil || result.Text != "ok") {
				t.Fatal(result, err)
			}
			rows := piDiagnosticRows(t, s)
			finish := rows[len(rows)-1]
			want := "completed"
			if cancelRun {
				want = "cancelled"
			}
			if finish["status"] != want || finish["level"] != "info" {
				t.Fatal(finish)
			}
		})
	}
}

func TestPiWorkerRetryVisibilityResetsPerMessage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		preceding []piEvent
		accepted  bool
	}{
		{"reasoning and tool round", []piEvent{
			{Type: "reasoning", Text: "PRIVATE_REASONING"},
			{Type: "draft", Reset: true},
			{Type: "tool", ID: "read-1", Name: "read_passage", Args: json.RawMessage(`{}`)},
		}, true},
		{"draft reset", []piEvent{
			{Type: "draft", Text: "PRIVATE_DRAFT"},
			{Type: "draft", Reset: true},
		}, true},
		{"current draft", []piEvent{{Type: "draft", Text: "PRIVATE_DRAFT"}}, false},
		{"current reasoning", []piEvent{{Type: "reasoning", Text: "PRIVATE_REASONING"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t)
			logs, err := diagnostics.New(filepath.Join(s.Store.Root, "logs"), "pi-test")
			if err != nil {
				t.Fatal(err)
			}
			s.SetDiagnostics(logs)
			sequence := append(tc.preceding,
				piEvent{Type: "retry", Attempt: 1, Max: 3, Status: 503, DelayMs: 1000, Attempts: 2},
				piEvent{Type: "complete", Text: "ok", Entries: json.RawMessage(`[]`), Attempts: 3},
			)
			piFakeWorker(t, s, sequence...)
			activity := false
			result, err := s.runPiWorker(t.Context(), s.Store.Root, piRequest{RunID: "visibility-test"}, func(e piEvent) error {
				if e.Type == "activity" {
					activity = true
				}
				return nil
			}, func(_ string, _ json.RawMessage) ([]any, error) {
				return []any{map[string]string{"type": "text", "text": "tool result"}}, nil
			})
			if tc.accepted {
				if err != nil || result.Type != "complete" || !activity {
					t.Fatalf("retry after reset failed: %+v %v", result, err)
				}
			} else if err == nil || errorKind(err) != ErrorProtocol || !strings.Contains(err.Error(), "无效的重试事件") || result.Type != "" || activity {
				t.Fatalf("retry after current output was accepted: %+v %v", result, err)
			}
			rows := piDiagnosticRows(t, s)
			finish := rows[len(rows)-1]
			if finish["visibleOutput"] != true || finish["currentMessageVisible"] != !tc.accepted {
				t.Fatal(finish)
			}
			if tc.accepted {
				retry := rows[1]
				if retry["event"] != "pi.request.retry" || retry["visibleOutput"] != true || retry["currentMessageVisible"] != false {
					t.Fatal(retry)
				}
			}
		})
	}
}
