package neoread

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type webRoundTrip func(*http.Request) (*http.Response, error)

func (f webRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func webJSON(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}
func TestDeepSeekWebPermissionSourcesAndFailure(t *testing.T) {
	conn := APIConnection{URL: "https://api.deepseek.com/v1", Key: "test-secret"}
	calls := 0
	c := &chatInput{WebSearch: true, webTransport: webRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://api.deepseek.com/v1/tools/search" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Fatal("wrong destination or credential")
		}
		data, _ := io.ReadAll(r.Body)
		if strings.Contains(string(data), "ori_question") || strings.Contains(string(data), "PRIVATE_DOCUMENT") {
			t.Fatal("private context sent")
		}
		return webJSON(200, `{"results":[{"title":"Official","url":"https://example.com/article","summary":"Search summary","date":"2026-10-23"},{"title":"local","url":"http://127.0.0.1/private"}]}`), nil
	})}
	raw, err := c.callWebTool(t.Context(), conn, "web_search", json.RawMessage(`{"query":"official documentation"}`))
	if err != nil || !strings.Contains(string(raw), "search_summaries") {
		t.Fatal(string(raw), err)
	}
	if calls != 1 || len(c.WebSources) != 1 || c.WebSources[0].Kind != "search" || c.WebSources[0].RetrievedAt == "" || c.Tools[0].Status != "completed" {
		t.Fatal(c.WebSources, c.Tools)
	}
	c.WebSearch = false
	if _, err = c.callWebTool(t.Context(), conn, "web_search", json.RawMessage(`{"query":"x"}`)); errorKind(err) != ErrorPermission || calls != 1 {
		t.Fatal("disabled tool made a request", err)
	}
	c.WebSearch = true
	c.webTransport = webRoundTrip(func(*http.Request) (*http.Response, error) { return webJSON(429, `{"error":"PRIVATE_SECRET"}`), nil })
	raw, err = c.callWebTool(t.Context(), conn, "web_search", json.RawMessage(`{"query":"x"}`))
	if err != nil || !strings.Contains(string(raw), "限流") || strings.Contains(string(raw), "PRIVATE_SECRET") || c.Tools[1].Status != "failed" {
		t.Fatal(string(raw), err, c.Tools)
	}
	c.webCalls = webToolCalls
	if raw, err = c.callWebTool(t.Context(), conn, "web_search", json.RawMessage(`{"query":"x"}`)); err != nil || !strings.Contains(string(raw), `"budgetNotice"`) {
		t.Fatal(string(raw), err)
	}
}
func TestDeepSeekWebFetchBoundsAndCancel(t *testing.T) {
	conn := APIConnection{URL: "https://api.deepseek.com", Key: "test"}
	c := &chatInput{WebSearch: true, webTransport: webRoundTrip(func(r *http.Request) (*http.Response, error) {
		return webJSON(200, jsonText(map[string]string{"title": "Page", "content": strings.Repeat("a", 9000)})), nil
	})}
	raw, err := c.callWebTool(t.Context(), conn, "web_fetch", json.RawMessage(`{"url":"https://1.1.1.1/page"}`))
	if err != nil || !strings.Contains(string(raw), `"truncated":true`) || len(c.WebSources) != 1 || c.WebSources[0].Kind != "page" {
		t.Fatal(err, string(raw))
	}
	for _, url := range []string{"file:///etc/passwd", "http://127.0.0.1/", "http://[::1]/", "http://192.168.1.1/", "http://169.254.169.254/", "https://user:pass@example.com/"} {
		if err := validateWebTarget(t.Context(), url); errorKind(err) != ErrorPermission {
			t.Fatal(url, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.webTransport = webRoundTrip(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	if _, err = c.callWebTool(ctx, conn, "web_search", json.RawMessage(`{"query":"x"}`)); err == nil || c.Tools[len(c.Tools)-1].Status != "cancelled" {
		t.Fatal(err, c.Tools)
	}
}
func TestWebConnectionAndRedirectBoundaries(t *testing.T) {
	for _, raw := range []string{"https://api.deepseek.com.evil.test", "http://api.deepseek.com", "https://api.deepseek.com:8443", "https://u:p@api.deepseek.com", "https://api.deepseek.com/v1?token=x"} {
		if deepSeekSearchConnection(APIConnection{URL: raw, Key: "key"}) {
			t.Fatal(raw)
		}
	}
	transport := webRoundTrip(func(*http.Request) (*http.Response, error) {
		r := webJSON(302, "")
		r.Header.Set("Location", "https://other.example/leak")
		return r, nil
	})
	if _, err := deepSeekWebRequest(t.Context(), APIConnection{URL: defaultSearchURL, Key: "secret"}, "search", map[string]string{"query": "x"}, transport); err == nil {
		t.Fatal("redirect accepted")
	}
	if got := displayWebURL("https://example.com/page?token=private&q=reader#secret"); strings.Contains(got, "private") || strings.Contains(got, "secret") || !strings.Contains(got, "q=reader") {
		t.Fatal(got)
	}
}
func TestNativeToolActivitiesAreScopedAndPersistent(t *testing.T) {
	c := &chatInput{WebSearch: true}
	start := json.RawMessage(`{"item":{"id":"search-1","type":"webSearch","query":"official docs","action":{"type":"search","queries":["official docs"]}}}`)
	if err := c.codexToolEvent("item/started", start); err != nil {
		t.Fatal(err)
	}
	if err := c.codexToolEvent("item/completed", json.RawMessage(`{"item":{"id":"search-1","type":"webSearch","query":"official docs","results":[{"url":"https://example.com/docs","title":"Official"}]}}`)); err != nil {
		t.Fatal(err)
	}
	if len(c.Tools) != 1 || c.Tools[0].Status != "completed" || len(c.WebSources) != 1 {
		t.Fatal(c.Tools, c.WebSources)
	}
	c.recordTool(ToolActivity{ID: "mcp-1", Kind: "tool", Tool: "reader/read_passage", Title: "读取原文", Status: "running"})
	c.codexToolEvent("item/mcpToolCall/progress", json.RawMessage(`{"itemId":"mcp-1","message":"Reading chapter 2"}`))
	if c.Tools[1].Detail != "Reading chapter 2" {
		t.Fatal(c.Tools)
	}
	c.settleTools("cancelled")
	if c.Tools[1].Status != "cancelled" || c.Tools[0].Status != "completed" {
		t.Fatal(c.Tools)
	}
	c.WebSearch = false
	if err := c.codexToolEvent("item/started", start); errorKind(err) != ErrorPermission {
		t.Fatal("unauthorized native search", err)
	}
	s, a := originalDocumentFixture(t)
	a.chat = c
	if err := s.saveDocumentAgentAnswer(t.Context(), a, "Answer without document claims", "codex"); err != nil {
		t.Fatal(err)
	}
	history, err := s.Store.chatHistory("doc")
	if err != nil || len(history) != 1 || len(history[0].Tools) != 2 || len(history[0].WebSources) != 1 {
		t.Fatal(history, err)
	}
}

func TestPiDeepSeekWebToolsBridge(t *testing.T) {
	piTestRuntime(t)
	s, a := originalDocumentFixture(t)
	preload := filepath.Join(t.TempDir(), "mock-model.cjs")
	script := `globalThis.fetch=async function(url,init){
 if(new Headers(init.headers).get('authorization')!=='Bearer model-secret')throw new Error('model credential missing');
 const p=JSON.parse(init.body),tools=p.messages.filter(m=>m.role==='tool');
 if(init.body.includes('search-secret'))throw new Error('search key leaked to model');
 if(!p.tools?.some(t=>t.function.name==='web_search'))throw new Error('web tool missing');
 const call=tools.length===0?{name:'web_search',arguments:JSON.stringify({query:'official docs'})}:tools.length===1?{name:'web_fetch',arguments:JSON.stringify({url:'https://1.1.1.1/docs'})}:null;
 const delta=call?{tool_calls:[{index:0,id:'tool-'+tools.length,type:'function',function:call}]}:{content:'See [Official](https://1.1.1.1/docs).'};
 let out='';for(const choice of [{delta,finish_reason:null},{delta:{},finish_reason:call?'tool_calls':'stop'}])out+='data: '+JSON.stringify({id:'test',object:'chat.completion.chunk',model:'test',choices:[{index:0,...choice}]})+'\n\n';
 return new Response(out+'data: [DONE]\n\n',{headers:{'Content-Type':'text/event-stream'}});
 };`
	if err := os.WriteFile(preload, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_OPTIONS", "--require="+preload)
	calls := []string{}
	chat := &chatInput{server: s, DocumentID: "doc", SessionID: "", Question: "Find official docs", Agent: a, WebSearch: true, WebAPI: APIConnection{URL: "https://search.example/gateway/v1", Key: "search-secret"}, webTransport: webRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "search.example" || !strings.HasPrefix(r.URL.Path, "/gateway/v1/tools/") || r.Header.Get("Authorization") != "Bearer search-secret" {
			t.Fatal("search connection was mixed with the model connection")
		}
		calls = append(calls, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "search") {
			return webJSON(200, `{"results":[{"title":"Official","url":"https://1.1.1.1/docs","summary":"Search candidate"}]}`), nil
		}
		return webJSON(200, `{"title":"Official","content":"Verified public page text"}`), nil
	})}
	result, err := streamPiChat(t.Context(), APIConnection{URL: "https://model.example/v1", Key: "model-secret"}, completionRequest{Model: "test", Tools: true, ContextWindow: 64000}, AIInput{Prompt: chat.Question, Chat: chat}, nil)
	if err != nil || !strings.Contains(result.Text, "https://1.1.1.1/docs") || len(calls) != 2 || len(chat.Tools) != 2 || len(chat.WebSources) != 1 || chat.WebSources[0].Kind != "page" || chat.WebSources[0].Text != "Verified public page text" {
		t.Fatal(err, result.Text, calls, chat.Tools, chat.WebSources)
	}
	if err = s.saveDocumentAgentAnswer(t.Context(), a, result.Text, "api"); err != nil {
		t.Fatal(err)
	}
	history, err := s.Store.chatHistory("doc")
	if err != nil || len(history) != 1 || len(history[0].WebSources) != 1 || len(history[0].Tools) != 2 {
		t.Fatal(err, history)
	}
}

func TestNativeWebCitationsKeepDocumentValidationAndStreaming(t *testing.T) {
	c := &chatInput{WebSearch: true}
	c.rememberNativeWebSource("turn0search0", "https://example.com/docs")
	text := "Before citeturn0search0S2 after"
	want := "Before [网页来源](https://example.com/docs)[S2] after"
	stream := webCitationStream{normalize: c.normalizeWebCitations}
	out := ""
	for _, r := range text {
		out += stream.feed(string(r))
	}
	out += stream.finish()
	if out != want {
		t.Fatal(out)
	}
	ids, err := documentCitationSources(out)
	if err != nil || len(ids) != 1 || ids[0] != "S2" {
		t.Fatal(ids, err)
	}
	if got := c.normalizeWebCitations("citeturn1view0"); !strings.Contains(got, "引用未解析") {
		t.Fatal(got)
	}
	if _, err := documentCitationSources(c.normalizeWebCitations("citeunknown")); err == nil {
		t.Fatal("unknown source bypassed validator")
	}
	c.WebSearch = false
	if c.normalizeWebCitations(text) != text {
		t.Fatal("disabled mode changed validation")
	}
}

func TestSourceRefreshStillEnforcesSnapshotLimit(t *testing.T) {
	c := &chatInput{}
	first := c.addWebSource(WebSource{URL: "https://example.com/", Title: "Original", Text: "Summary", Kind: "search"})
	next := c.addWebSource(WebSource{URL: first.URL, Title: strings.Repeat("x", 500), Text: strings.Repeat("原文", 8000), Kind: "page"})
	if next.ID != first.ID || !next.Truncated || len(next.Text) > 8192 || len([]rune(next.Title)) > 161 || next.RetrievedAt == "" {
		t.Fatal(next.ID, next.Truncated, len(next.Text), len([]rune(next.Title)))
	}
	c.recordTool(ToolActivity{ID: "x", Kind: "tool", Title: "Read", Status: "completed"})
	c.recordTool(ToolActivity{ID: "x", Kind: "tool", Title: "Read", Status: "running"})
	if c.Tools[0].Status != "completed" {
		t.Fatal("late start reverted completion")
	}
}

func TestToolCompletionRetainsActualTargetAndProgress(t *testing.T) {
	c := &chatInput{}
	c.recordTool(ToolActivity{ID: "m", Kind: "tool", Tool: "reader/read_passage", Title: "调用工具", Target: "read_passage", Status: "running"})
	step := c.Tools[0]
	step.Target = "正在读取第 7 页"
	c.recordTool(step)
	c.codexToolEvent("item/mcpToolCall/progress", json.RawMessage(`{"itemId":"m","message":"已读取第 7 页"}`))
	c.codexToolEvent("item/completed", json.RawMessage(`{"item":{"id":"m","type":"mcpToolCall","server":"reader","tool":"read_passage","status":"completed","arguments":{"blockId":"p7-b1"}}}`))
	if c.Tools[0].Target != "正在读取第 7 页" || c.Tools[0].Detail != "已读取第 7 页" || c.Tools[0].Status != "completed" {
		t.Fatal(c.Tools)
	}
	s, a := originalDocumentFixture(t)
	a.chat = c
	if err := s.saveDocumentAgentAnswer(t.Context(), a, "Summary", "codex"); err != nil {
		t.Fatal(err)
	}
	messages, err := s.Store.chatHistory("doc")
	if err != nil || messages[0].Tools[0].Target != c.Tools[0].Target || messages[0].Tools[0].Detail != c.Tools[0].Detail {
		t.Fatal(messages, err)
	}
}
func TestWebSourceBudgetIsNotReportedAsNoResults(t *testing.T) {
	c := &chatInput{WebSearch: true}
	for i := 0; i < webSourceLimit-1; i++ {
		c.addWebSource(WebSource{URL: fmt.Sprintf("https://example.com/%d", i), Title: "Existing", Kind: "search"})
	}
	count := 0
	c.webTransport = webRoundTrip(func(*http.Request) (*http.Response, error) {
		count++
		return webJSON(200, `{"results":[{"title":"A","url":"https://example.com/a"},{"title":"B","url":"https://example.com/b"}]}`), nil
	})
	conn := APIConnection{URL: "https://api.deepseek.com", Key: "dummy"}
	raw, err := c.callWebTool(t.Context(), conn, "web_search", json.RawMessage(`{"query":"new docs"}`))
	if err != nil || !strings.Contains(string(raw), `"budgetExhausted":true`) || !strings.Contains(c.Tools[0].Detail, "预算已满") {
		t.Fatal(string(raw), c.Tools, err)
	}
	raw, err = c.callWebTool(t.Context(), conn, "web_search", json.RawMessage(`{"query":"other docs"}`))
	if err != nil || !strings.Contains(string(raw), `"shouldFinish":true`) || count != 1 || c.Tools[1].Status != "failed" {
		t.Fatal(string(raw), c.Tools, err, count)
	}
	err = c.codexToolEvent("item/completed", json.RawMessage(`{"item":{"id":"native","type":"webSearch","query":"other docs","results":[{"title":"New","url":"https://example.com/new"}]}}`))
	if err != nil || !strings.Contains(c.Tools[2].Detail, "预算已满") {
		t.Fatal(err, c.Tools)
	}
}

func TestPiWebBudgetLetsModelFinish(t *testing.T) {
	piTestRuntime(t)
	s, a := originalDocumentFixture(t)
	preload := filepath.Join(t.TempDir(), "mock-model.cjs")
	script := `globalThis.fetch=async function(url,init){
 if(new Headers(init.headers).get('authorization')!=='Bearer model-secret')throw new Error('model credential missing');
 const p=JSON.parse(init.body),tools=p.messages.filter(m=>m.role==='tool');
 if(init.body.includes('search-secret'))throw new Error('search key leaked to model');
 if(!p.tools?.some(t=>t.function.name==='web_search'))throw new Error('web tool missing');
 if(tools.length && !tools.at(-1).content.includes('budgetNotice'))throw new Error('missing budget notice');
 const call=tools.length===0?{name:'web_search',arguments:JSON.stringify({query:'official docs'})}:null;
 const delta=call?{tool_calls:[{index:0,id:'tool-'+tools.length,type:'function',function:call}]}:{content:'See [Official](https://1.1.1.1/docs).'};
 let out='';for(const choice of [{delta,finish_reason:null},{delta:{},finish_reason:call?'tool_calls':'stop'}])out+='data: '+JSON.stringify({id:'test',object:'chat.completion.chunk',model:'test',choices:[{index:0,...choice}]})+'\n\n';
 return new Response(out+'data: [DONE]\n\n',{headers:{'Content-Type':'text/event-stream'}});
 };`
	if err := os.WriteFile(preload, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_OPTIONS", "--require="+preload)
	calls := []string{}
	chat := &chatInput{server: s, DocumentID: "doc", SessionID: "", Question: "Find official docs", Agent: a, WebSearch: true, webCalls: 200, WebAPI: APIConnection{URL: "https://search.example/gateway/v1", Key: "search-secret"}, webTransport: webRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "search.example" || !strings.HasPrefix(r.URL.Path, "/gateway/v1/tools/") || r.Header.Get("Authorization") != "Bearer search-secret" {
			t.Fatal("search connection was mixed with the model connection")
		}
		calls = append(calls, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "search") {
			return webJSON(200, `{"results":[{"title":"Official","url":"https://1.1.1.1/docs","summary":"Search candidate"}]}`), nil
		}
		return webJSON(200, `{"title":"Official","content":"Verified public page text"}`), nil
	})}
	result, err := streamPiChat(t.Context(), APIConnection{URL: "https://model.example/v1", Key: "model-secret"}, completionRequest{Model: "test", Tools: true, ContextWindow: 64000}, AIInput{Prompt: chat.Question, Chat: chat}, nil)
	if err != nil || !strings.Contains(result.Text, "https://1.1.1.1/docs") || len(calls) != 1 {
		t.Fatal("model should finish after receiving budget notice", err, result.Text, calls)
	}
	if err := s.saveDocumentAgentAnswer(t.Context(), a, result.Text, "api"); err != nil {
		t.Fatal("final answer was not saved", err)
	}
	history, err := s.Store.chatHistory("doc")
	if err != nil || len(history) != 1 || !strings.Contains(history[0].Content, "Official") {
		t.Fatal("completed answer missing from history", history, err)
	}
}

func TestWebBudgetAllows200Requests(t *testing.T) {
	requests := 0
	c := &chatInput{WebSearch: true, webTransport: webRoundTrip(func(*http.Request) (*http.Response, error) {
		requests++
		return webJSON(200, `{"results":[{"title":"A","url":"https://example.com/a"}]}`), nil
	})}
	conn := APIConnection{URL: "https://api.deepseek.com", Key: "test"}
	for i := 0; i < webToolCalls+1; i++ {
		raw, err := c.callWebTool(t.Context(), conn, "web_search", json.RawMessage(`{"query":"test"}`))
		if err != nil || (i == webToolCalls && !strings.Contains(string(raw), `"budgetNotice"`)) {
			t.Fatalf("call %d: %s %v", i+1, raw, err)
		}
	}
	if requests != webToolCalls+1 {
		t.Fatalf("got %d upstream requests, want %d", requests, webToolCalls+1)
	}
}

func TestToolActivityBudgetDoesNotInterrupt(t *testing.T) {
	c := &chatInput{}
	for i := 0; i < 300; i++ {
		activity := ToolActivity{ID: fmt.Sprint(i), Title: "搜索网页", Status: "running"}
		if err := c.recordTool(activity); err != nil {
			t.Fatal(i, err)
		}
		activity.Status = "completed"
		if err := c.recordTool(activity); err != nil {
			t.Fatal(i, err)
		}
	}
	if len(c.Tools) != 256 || c.Tools[255].Status != "completed" {
		t.Fatal("record limit or retained activity state is incorrect", len(c.Tools))
	}
}

func TestPiToolRoundsFinishWithoutInterruption(t *testing.T) {
	piTestRuntime(t)
	s, a := originalDocumentFixture(t)
	preload := filepath.Join(t.TempDir(), "mock-model.cjs")
	script := `globalThis.fetch=async function(url,init){
 if(new Headers(init.headers).get('authorization')!=='Bearer model-secret')throw new Error('model credential missing');
 const p=JSON.parse(init.body),tools=p.messages.filter(m=>m.role==='tool');
 if(init.body.includes('search-secret'))throw new Error('search key leaked to model');
 if(!p.tools?.some(t=>t.function.name==='web_search'))throw new Error('web tool missing');
 const call=tools.length<64?{name:'web_search',arguments:JSON.stringify({query:'official docs'})}:null;
 const delta=call?{tool_calls:[{index:0,id:'tool-'+tools.length,type:'function',function:call}]}:{content:'See [Official](https://1.1.1.1/docs).'};
 let out='';for(const choice of [{delta,finish_reason:null},{delta:{},finish_reason:call?'tool_calls':'stop'}])out+='data: '+JSON.stringify({id:'test',object:'chat.completion.chunk',model:'test',choices:[{index:0,...choice}]})+'\n\n';
 return new Response(out+'data: [DONE]\n\n',{headers:{'Content-Type':'text/event-stream'}});
 };`
	if err := os.WriteFile(preload, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_OPTIONS", "--require="+preload)
	calls := []string{}
	chat := &chatInput{server: s, DocumentID: "doc", SessionID: "", Question: "Find official docs", Agent: a, WebSearch: true, WebAPI: APIConnection{URL: "https://search.example/gateway/v1", Key: "search-secret"}, webTransport: webRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "search.example" || !strings.HasPrefix(r.URL.Path, "/gateway/v1/tools/") || r.Header.Get("Authorization") != "Bearer search-secret" {
			t.Fatal("search connection was mixed with the model connection")
		}
		calls = append(calls, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "search") {
			return webJSON(200, `{"results":[{"title":"Official","url":"https://1.1.1.1/docs","summary":"Search candidate"}]}`), nil
		}
		return webJSON(200, `{"title":"Official","content":"Verified public page text"}`), nil
	})}
	result, err := streamPiChat(t.Context(), APIConnection{URL: "https://model.example/v1", Key: "model-secret"}, completionRequest{Model: "test", Tools: true, ContextWindow: 64000}, AIInput{Prompt: chat.Question, Chat: chat}, nil)
	if err != nil || !strings.Contains(result.Text, "https://1.1.1.1/docs") || len(calls) != 64 {
		t.Fatal("model should finish after receiving budget notice", err, result.Text, calls)
	}
	if err := s.saveDocumentAgentAnswer(t.Context(), a, result.Text, "api"); err != nil {
		t.Fatal("final answer was not saved", err)
	}
	history, err := s.Store.chatHistory("doc")
	if err != nil || len(history) != 1 || !strings.Contains(history[0].Content, "Official") {
		t.Fatal("completed answer missing from history", history, err)
	}
}

func TestPiRoundBudgetWarnsWithoutDisablingTools(t *testing.T) {
	piTestRuntime(t)
	s, a := originalDocumentFixture(t)
	preload := filepath.Join(t.TempDir(), "mock-model.cjs")
	script := `let round=0; globalThis.fetch=async function(url,init){
 if(new Headers(init.headers).get('authorization')!=='Bearer model-secret')throw new Error('model credential missing');
 const p=JSON.parse(init.body),tools=p.messages.filter(m=>m.role==='tool');
 if(init.body.includes('search-secret'))throw new Error('search key leaked to model');
 round++; if(round===256 && (!p.tools?.length || !JSON.stringify(p.messages).includes('预算')))throw new Error('tools disabled or budget warning missing');
 const call=round<=256?{name:'web_search',arguments:JSON.stringify({query:'official docs'})}:null;
 const delta=call?{tool_calls:[{index:0,id:'tool-'+tools.length,type:'function',function:call}]}:{content:'See [Official](https://1.1.1.1/docs).'};
 let out='';for(const choice of [{delta,finish_reason:null},{delta:{},finish_reason:call?'tool_calls':'stop'}])out+='data: '+JSON.stringify({id:'test',object:'chat.completion.chunk',model:'test',choices:[{index:0,...choice}]})+'\n\n';
 return new Response(out+'data: [DONE]\n\n',{headers:{'Content-Type':'text/event-stream'}});
 };`
	if err := os.WriteFile(preload, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_OPTIONS", "--require="+preload)
	calls := []string{}
	chat := &chatInput{server: s, DocumentID: "doc", SessionID: "", Question: "Find official docs", Agent: a, WebSearch: true, WebAPI: APIConnection{URL: "https://search.example/gateway/v1", Key: "search-secret"}, webTransport: webRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "search.example" || !strings.HasPrefix(r.URL.Path, "/gateway/v1/tools/") || r.Header.Get("Authorization") != "Bearer search-secret" {
			t.Fatal("search connection was mixed with the model connection")
		}
		calls = append(calls, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "search") {
			return webJSON(200, `{"results":[{"title":"Official","url":"https://1.1.1.1/docs","summary":"Search candidate"}]}`), nil
		}
		return webJSON(200, `{"title":"Official","content":"Verified public page text"}`), nil
	})}
	result, err := streamPiChat(t.Context(), APIConnection{URL: "https://model.example/v1", Key: "model-secret"}, completionRequest{Model: "test", Tools: true, ContextWindow: 64000}, AIInput{Prompt: chat.Question, Chat: chat}, nil)
	if err != nil || !strings.Contains(result.Text, "https://1.1.1.1/docs") || len(calls) != 256 {
		t.Fatal("model should finish after receiving budget notice", err, result.Text, calls)
	}
	if err := s.saveDocumentAgentAnswer(t.Context(), a, result.Text, "api"); err != nil {
		t.Fatal("final answer was not saved", err)
	}
	history, err := s.Store.chatHistory("doc")
	if err != nil || len(history) != 1 || !strings.Contains(history[0].Content, "Official") {
		t.Fatal("completed answer missing from history", history, err)
	}
}

func TestPiWebProtocolErrorLetsModelFinish(t *testing.T) {
	piTestRuntime(t)
	s, a := originalDocumentFixture(t)
	preload := filepath.Join(t.TempDir(), "mock-model.cjs")
	script := `globalThis.fetch=async function(url,init){
 if(new Headers(init.headers).get('authorization')!=='Bearer model-secret')throw new Error('model credential missing');
 const p=JSON.parse(init.body),tools=p.messages.filter(m=>m.role==='tool');
 if(init.body.includes('search-secret'))throw new Error('search key leaked to model');
 if(!p.tools?.some(t=>t.function.name==='web_search'))throw new Error('web tool missing');
 if(tools.length && !tools.at(-1).content.includes('web_tool_failed'))throw new Error('missing budget notice');
 const call=tools.length===0?{name:'web_search',arguments:JSON.stringify({query:'official docs'})}:null;
 const delta=call?{tool_calls:[{index:0,id:'tool-'+tools.length,type:'function',function:call}]}:{content:'See [Official](https://1.1.1.1/docs).'};
 let out='';for(const choice of [{delta,finish_reason:null},{delta:{},finish_reason:call?'tool_calls':'stop'}])out+='data: '+JSON.stringify({id:'test',object:'chat.completion.chunk',model:'test',choices:[{index:0,...choice}]})+'\n\n';
 return new Response(out+'data: [DONE]\n\n',{headers:{'Content-Type':'text/event-stream'}});
 };`
	if err := os.WriteFile(preload, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_OPTIONS", "--require="+preload)
	calls := []string{}
	chat := &chatInput{server: s, DocumentID: "doc", SessionID: "", Question: "Find official docs", Agent: a, WebSearch: true, WebAPI: APIConnection{URL: "https://search.example/gateway/v1", Key: "search-secret"}, webTransport: webRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "search.example" || !strings.HasPrefix(r.URL.Path, "/gateway/v1/tools/") || r.Header.Get("Authorization") != "Bearer search-secret" {
			t.Fatal("search connection was mixed with the model connection")
		}
		calls = append(calls, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "search") {
			return webJSON(200, `{}`), nil
		}
		return webJSON(200, `{"title":"Official","content":"Verified public page text"}`), nil
	})}
	result, err := streamPiChat(t.Context(), APIConnection{URL: "https://model.example/v1", Key: "model-secret"}, completionRequest{Model: "test", Tools: true, ContextWindow: 64000}, AIInput{Prompt: chat.Question, Chat: chat}, nil)
	if err != nil || !strings.Contains(result.Text, "https://1.1.1.1/docs") || len(calls) != 1 {
		t.Fatal("model should finish after receiving budget notice", err, result.Text, calls)
	}
	if err := s.saveDocumentAgentAnswer(t.Context(), a, result.Text, "api"); err != nil {
		t.Fatal("final answer was not saved", err)
	}
	history, err := s.Store.chatHistory("doc")
	if err != nil || len(history) != 1 || !strings.Contains(history[0].Content, "Official") {
		t.Fatal("completed answer missing from history", history, err)
	}
}

func TestWebToolUsesExtendedDeadline(t *testing.T) {
	c := &chatInput{WebSearch: true, webTransport: webRoundTrip(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) < 119*time.Second || time.Until(deadline) > 2*time.Minute {
			t.Errorf("unexpected web deadline %v", deadline)
		}
		return webJSON(200, `{"results":[]}`), nil
	})}
	if _, err := c.callWebTool(context.Background(), APIConnection{URL: "https://api.deepseek.com", Key: "test"}, "web_search", json.RawMessage(`{"query":"test"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestPiOversizedToolArgumentsLetModelFinish(t *testing.T) {
	piTestRuntime(t)
	s, a := originalDocumentFixture(t)
	preload := filepath.Join(t.TempDir(), "mock-model.cjs")
	script := `globalThis.fetch=async function(url,init){
 if(new Headers(init.headers).get('authorization')!=='Bearer model-secret')throw new Error('model credential missing');
 const p=JSON.parse(init.body),tools=p.messages.filter(m=>m.role==='tool');
 if(init.body.includes('search-secret'))throw new Error('search key leaked to model');
 if(!p.tools?.some(t=>t.function.name==='web_search'))throw new Error('web tool missing');
 if(tools.length && !/validation|invalid|error/i.test(tools.at(-1).content))throw new Error('missing budget notice');
 const call=tools.length===0?{name:'web_search',arguments:JSON.stringify({query:'x'.repeat(5000)})}:null;
 const delta=call?{tool_calls:[{index:0,id:'tool-'+tools.length,type:'function',function:call}]}:{content:'See [Official](https://1.1.1.1/docs).'};
 let out='';for(const choice of [{delta,finish_reason:null},{delta:{},finish_reason:call?'tool_calls':'stop'}])out+='data: '+JSON.stringify({id:'test',object:'chat.completion.chunk',model:'test',choices:[{index:0,...choice}]})+'\n\n';
 return new Response(out+'data: [DONE]\n\n',{headers:{'Content-Type':'text/event-stream'}});
 };`
	if err := os.WriteFile(preload, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_OPTIONS", "--require="+preload)
	calls := []string{}
	chat := &chatInput{server: s, DocumentID: "doc", SessionID: "", Question: "Find official docs", Agent: a, WebSearch: true, WebAPI: APIConnection{URL: "https://search.example/gateway/v1", Key: "search-secret"}, webTransport: webRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "search.example" || !strings.HasPrefix(r.URL.Path, "/gateway/v1/tools/") || r.Header.Get("Authorization") != "Bearer search-secret" {
			t.Fatal("search connection was mixed with the model connection")
		}
		calls = append(calls, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "search") {
			return webJSON(200, `{}`), nil
		}
		return webJSON(200, `{"title":"Official","content":"Verified public page text"}`), nil
	})}
	result, err := streamPiChat(t.Context(), APIConnection{URL: "https://model.example/v1", Key: "model-secret"}, completionRequest{Model: "test", Tools: true, ContextWindow: 64000}, AIInput{Prompt: chat.Question, Chat: chat}, nil)
	if err != nil || !strings.Contains(result.Text, "https://1.1.1.1/docs") || len(calls) != 0 {
		t.Fatal("model should finish after receiving budget notice", err, result.Text, calls)
	}
	if err := s.saveDocumentAgentAnswer(t.Context(), a, result.Text, "api"); err != nil {
		t.Fatal("final answer was not saved", err)
	}
	history, err := s.Store.chatHistory("doc")
	if err != nil || len(history) != 1 || !strings.Contains(history[0].Content, "Official") {
		t.Fatal("completed answer missing from history", history, err)
	}
}

func TestSearchSettingsKeepSecretsSeparateAndPreserveOmittedChanges(t *testing.T) {
	s := testServer(t)
	c := s.aiConfig()
	c.API = APIConnection{URL: "https://model.example/v1", Key: "model-secret"}
	c.WebSearch = &WebSearchConfig{CLIEnabled: true, Provider: "deepseek", URL: "https://search.example/v1", Key: "search-secret"}
	save := func(config AIConfig, want int) {
		t.Helper()
		w := request(t, s, "PUT", "/api/ai/config", strings.NewReader(jsonText(config)))
		if w.Code != want {
			t.Fatalf("save status %d, want %d", w.Code, want)
		}
		if strings.Contains(w.Body.String(), "search-secret") || strings.Contains(w.Body.String(), "model-secret") {
			t.Fatal("configuration response exposes a key")
		}
	}
	save(c, 200)
	stored := s.aiConfig()
	public := publicConfig(stored)
	if public.WebSearch.Key != "" || !public.WebSearch.HasKey || stored.WebSearch.Key != "search-secret" {
		t.Fatal("public configuration damaged or exposed the saved search key")
	}
	public.WebSearch = nil
	save(public, 200)
	if s.aiConfig().WebSearch.Key != "search-secret" {
		t.Fatal("unrelated save dropped search settings")
	}
	public = publicConfig(s.aiConfig())
	public.WebSearch.CLIEnabled = false
	save(public, 200)
	if s.aiConfig().WebSearch.Key != "search-secret" || s.aiConfig().API.Key != "model-secret" {
		t.Fatal("masked save dropped a key")
	}
	public.WebSearch.URL = "https://new-search.example"
	save(public, 400)
	if s.aiConfig().WebSearch.URL != "https://search.example/v1" {
		t.Fatal("rejected endpoint was saved")
	}
	public.WebSearch.Key = "replacement-key"
	save(public, 200)
	if s.aiConfig().WebSearch.Key != "replacement-key" || s.aiConfig().API.Key != "model-secret" {
		t.Fatal("replacement crossed the model/search boundary")
	}
	public = publicConfig(s.aiConfig())
	public.WebSearch.Provider = ""
	public.WebSearch.HasKey = false
	save(public, 200)
	if s.aiConfig().WebSearch.Key != "" || webSearchEnabled(s.aiConfig(), "api") {
		t.Fatal("explicit key removal did not disable search")
	}
}

func TestSearchSettingsValidationAndSavedPermissions(t *testing.T) {
	for _, config := range []WebSearchConfig{
		{Provider: "unsupported", Key: "key"},
		{Provider: "deepseek", URL: "http://remote.example", Key: "key"},
		{Provider: "deepseek", URL: "https://u:p@search.example", Key: "key"},
		{Provider: "deepseek", URL: "https://search.example?token=x", Key: "key"},
		{Provider: "deepseek", Key: "line1\nline2"},
	} {
		if mergeWebSearchConfig(&config, nil) == nil {
			t.Fatal("invalid search configuration accepted")
		}
	}
	c := AIConfig{API: APIConnection{URL: defaultSearchURL, Key: "model-secret"}}
	if webSearchEnabled(c, "api") || webSearchEnabled(c, "codex") {
		t.Fatal("model connection implicitly authorized search")
	}
	incomplete := &WebSearchConfig{Provider: "deepseek"}
	if mergeWebSearchConfig(incomplete, nil) != nil {
		t.Fatal("cannot select a search provider before entering its key")
	}
	c.WebSearch = incomplete
	if webSearchEnabled(c, "api") {
		t.Fatal("selecting a provider without a key authorized search")
	}
	c.WebSearch = &WebSearchConfig{Provider: "deepseek", URL: "http://127.0.0.1:1234/v1", Key: "search-secret"}
	if !webSearchEnabled(c, "api") || webSearchEnabled(c, "codex") || webSearchEnabled(c, "claude") || webSearchEnabled(c, "kimi") {
		t.Fatal("permissions crossed provider boundaries")
	}
	c.WebSearch.CLIEnabled = true
	if !webSearchEnabled(c, "codex") {
		t.Fatal("CLI setting was ignored")
	}
	encoded, _ := json.Marshal(publicConfig(c))
	if strings.Contains(string(encoded), "search-secret") {
		t.Fatal("search key exposed")
	}
}

type liveWebCase struct {
	ReportTextLimitBytes int            `json:"reportTextLimitBytes"`
	Provider             string         `json:"provider"`
	Scenario             string         `json:"scenario"`
	Status               string         `json:"status"`
	ElapsedMs            int64          `json:"elapsedMs"`
	CancelRequested      bool           `json:"cancelRequested"`
	Messages             []Message      `json:"messages"`
	Events               []chatRunEvent `json:"events"`
}

// Opt in explicitly: these tests call real model/search services using only a
// public query and an isolated synthetic document. No real library is modified.
func TestLiveWebSearchLifecycle(t *testing.T) {
	providers := os.Getenv("NEOREAD_TEST_WEB_PROVIDERS")
	if providers == "" {
		t.Skip("set NEOREAD_TEST_WEB_PROVIDERS=codex,api to authorize real model and search calls")
	}
	reports := []liveWebCase{}
	defer func() {
		if path := os.Getenv("NEOREAD_TEST_WEB_REPORT"); path != "" {
			data, _ := json.MarshalIndent(reports, "", "  ")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Error("cannot save sanitized live report")
			}
		}
	}()
	for _, provider := range strings.Split(providers, ",") {
		if provider != "codex" && provider != "api" {
			t.Fatal("unsupported live provider")
		}
		t.Run(provider, func(t *testing.T) {
			cfg := AIConfig{Primary: provider, Models: map[string]string{}, Capabilities: map[string]Capability{}}
			if provider == "api" {
				path := os.Getenv("NEOREAD_TEST_DOCUMENT_API_CONFIG")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal("existing NeoRead API connection unavailable")
				}
				var stored savedConfig
				if json.Unmarshal(raw, &stored) != nil || !deepSeekSearchConnection(stored.Config.API) {
					t.Fatal("live test requires an existing official DeepSeek connection")
				}
				cfg.API = stored.Config.API
				model := stored.Config.Models["api"]
				if model == "" {
					t.Fatal("select an existing DeepSeek model")
				}
				cfg.Models["api"] = model
				cfg.APIModels = []string{model}
				cfg.Efforts = stored.Config.Efforts
				probeCtx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
				status, err := probeAPIDocumentTools(probeCtx, cfg.API, model)
				cancel()
				if err != nil || status != "passed" {
					t.Fatalf("real tool handshake failed: %s %v", status, err)
				}
				cap := Capability{Text: true, Tools: "passed", ToolSchemaVersion: documentToolSchemaVersion, CheckedAt: now()}
				cap.Fingerprint = configPrint(cfg, apiCapabilityKey(model))
				cfg.Capabilities[apiCapabilityKey(model)] = cap
			}
			cfg.WebSearch = &WebSearchConfig{CLIEnabled: provider == "codex"}
			if provider == "api" {
				cfg.WebSearch.Provider = "deepseek"
				cfg.WebSearch.URL = defaultSearchURL
				cfg.WebSearch.Key = cfg.API.Key
			}
			s, _ := prepareConversationFixture(t)
			s.Token = id() + id()
			if err := s.writeAIConfig(cfg); err != nil {
				t.Fatal(err)
			}
			if provider == "api" {
				piTestRuntime(t)
			}
			server := httptest.NewServer(s.Handler())
			defer server.Close()
			client := &http.Client{Timeout: 4 * time.Minute}
			call := func(method, path string, body any) *http.Response {
				t.Helper()
				var reader io.Reader
				if body != nil {
					data, _ := json.Marshal(body)
					reader = bytes.NewReader(data)
				}
				req, err := http.NewRequest(method, server.URL+path, reader)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+s.Token)
				req.Header.Set("Content-Type", "application/json")
				res, err := client.Do(req)
				if err != nil {
					t.Fatal("isolated NeoRead request failed", err)
				}
				if res.StatusCode >= 400 {
					defer res.Body.Close()
					data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
					t.Fatalf("NeoRead returned %d: %s", res.StatusCode, data)
				}
				return res
			}
			for _, scenario := range []string{"search-and-read", "cancel"} {
				start := time.Now()
				entry := liveWebCase{Provider: provider, Scenario: scenario, ReportTextLimitBytes: 400}
				reports = append(reports, entry)
				idx := len(reports) - 1
				res := call("POST", "/api/documents/doc/chat-sessions", map[string]string{"provider": provider})
				var session ChatSession
				err := json.NewDecoder(res.Body).Decode(&session)
				res.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				prompt := "请完成一次真实联网验证：先搜索 DeepSeek Search Fetch API 官方文档，再实际读取 https://api-docs.deepseek.com/zh-cn/guides/search/ 的正文。两步都必须调用工具，不能只凭已有知识。不要读取本地文件，不必检索当前合成文档。最后用两句中文说明 Search 与 Fetch 的区别，给出两个 endpoint，附该网页的 Markdown 链接。最多一次搜索和一次网页读取。"
				if scenario == "cancel" {
					prompt = "先联网搜索 DeepSeek Search Fetch API 官方文档，然后依次读取 https://api-docs.deepseek.com/zh-cn/guides/search/ 和 https://api-docs.deepseek.com/zh-cn/api/search/ ，全部读取后再回答。必须实际调用网页工具，不读取本地文件。"
				}
				res = call("POST", "/api/documents/doc/chat-sessions/"+session.ID+"/chat", map[string]any{"provider": provider, "prompt": prompt, "context": "", "webSearch": true, "userMessageId": id()})
				var run ChatRun
				err = json.NewDecoder(res.Body).Decode(&run)
				res.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				res = call("GET", "/api/documents/doc/chat-sessions/"+session.ID+"/runs/"+run.ID+"/events", nil)
				events := []chatRunEvent{}
				name := ""
				var data strings.Builder
				scanner := bufio.NewScanner(res.Body)
				scanner.Buffer(make([]byte, 4096), 4<<20)
				terminal := false
				for scanner.Scan() {
					line := scanner.Text()
					if strings.HasPrefix(line, "event:") {
						name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
					}
					if strings.HasPrefix(line, "data:") {
						data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
					}
					if line != "" || name == "" {
						continue
					}
					raw := json.RawMessage(data.String())
					events = append(events, chatRunEvent{name, append(json.RawMessage(nil), raw...)})
					if name == "tool" {
						var tool ToolActivity
						json.Unmarshal(raw, &tool)
						t.Logf("provider=%s tool=%s status=%s", provider, tool.Kind, tool.Status)
					}
					if scenario == "cancel" && name == "tool" && !reports[idx].CancelRequested {
						var tool ToolActivity
						json.Unmarshal(raw, &tool)
						if tool.Status == "running" && (tool.Kind == "search" || tool.Kind == "web") {
							stop := call("POST", "/api/documents/doc/chat-sessions/"+session.ID+"/runs/"+run.ID+"/cancel", nil)
							stop.Body.Close()
							reports[idx].CancelRequested = true
						}
					}
					terminal = name == "done" || name == "error"
					name = ""
					data.Reset()
					if terminal {
						break
					}
				}
				scanErr := scanner.Err()
				res.Body.Close()
				reports[idx].ElapsedMs = time.Since(start).Milliseconds()
				// Keep only UI execution/answer events; never export provider thinking.
				for _, e := range events {
					if e.Event != "reasoning" {
						reports[idx].Events = append(reports[idx].Events, e)
					}
				}
				record, err := s.Store.readChatRun(session.ID, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				reports[idx].Status = record.Status
				messages, err := s.Store.chatHistory("doc", session.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range messages {
					m.Reasoning = ""
					for i := range m.WebSources {
						if len(m.WebSources[i].Text) > 400 {
							m.WebSources[i].Text = truncateChatText(m.WebSources[i].Text, 400)
						}
					}
					reports[idx].Messages = append(reports[idx].Messages, m)
				}
				if scanErr != nil || !terminal {
					t.Fatalf("live event stream incomplete: %v", scanErr)
				}
				if scenario == "cancel" {
					if !reports[idx].CancelRequested || record.Status != "cancelled" || len(messages) != 1 {
						t.Fatalf("cancellation did not stop the run: requested=%v status=%s messages=%d", reports[idx].CancelRequested, record.Status, len(messages))
					}
					for _, e := range events {
						if e.Event == "done" {
							t.Fatal("cancelled run falsely completed")
						}
					}
					t.Logf("provider=%s cancel status=%s elapsed=%dms", provider, record.Status, reports[idx].ElapsedMs)
					continue
				}
				if record.Status != "completed" || len(messages) != 2 {
					t.Fatalf("live answer not saved: status=%s error=%s messages=%d", record.Status, record.Error, len(messages))
				}
				answer := messages[1]
				searched, read := false, false
				for _, v := range answer.Tools {
					if v.Status == "completed" && v.Kind == "search" {
						searched = true
					}
					if v.Status == "completed" && v.Kind == "web" {
						read = true
					}
				}
				if !searched || !read || len(answer.WebSources) == 0 {
					t.Fatalf("missing real search/read events: search=%v read=%v sources=%d tools=%s", searched, read, len(answer.WebSources), jsonText(answer.Tools))
				}
				reopened, err := OpenStore(s.Store.Root)
				if err != nil {
					t.Fatal(err)
				}
				saved, err := reopened.chatHistory("doc", session.ID)
				reopened.DB.Close()
				if err != nil || len(saved) != 2 || len(saved[1].Tools) != len(answer.Tools) || len(saved[1].WebSources) != len(answer.WebSources) {
					t.Fatal("execution history did not survive reopening")
				}
				t.Logf("provider=%s saved=true search=true read=true tools=%d sources=%d elapsed=%dms", provider, len(answer.Tools), len(answer.WebSources), reports[idx].ElapsedMs)
			}
		})
	}
}

func runWebEventsProcess() {
	web := "disabled"
	isolated := false
	for _, arg := range os.Args {
		if arg == `web_search="live"` {
			web = "live"
		}
		if arg == "mcp_servers.unrelated.enabled=false" {
			isolated = true
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var r codexRPCMessage
		if json.Unmarshal(scanner.Bytes(), &r) != nil {
			os.Exit(2)
		}
		reply := func(v any) { encoder.Encode(map[string]any{"id": r.ID, "result": v}) }
		switch r.Method {
		case "initialize":
			reply(map[string]any{})
		case "config/read":
			reply(map[string]any{"config": map[string]any{"mcp_servers": map[string]any{"unrelated": map[string]any{"enabled": !isolated}}, "features": map[string]bool{}, "web_search": web}})
		case "model/list":
			reply(map[string]any{"data": []any{map[string]any{"model": "test", "isDefault": true}}})
		case "thread/start":
			if !isolated {
				os.Exit(3)
			}
			var p struct {
				Sandbox  string `json:"sandbox"`
				Approval string `json:"approvalPolicy"`
			}
			json.Unmarshal(r.Params, &p)
			if p.Sandbox != "read-only" || p.Approval != "never" {
				os.Exit(4)
			}
			reply(map[string]any{"thread": map[string]string{"id": "thread"}, "model": "test"})
		case "turn/start":
			reply(map[string]any{"turn": map[string]string{"id": "turn"}})
			emit := func(method string, item any) {
				encoder.Encode(map[string]any{"method": method, "params": map[string]any{"threadId": "thread", "turnId": "turn", "item": item}})
			}
			if mode := os.Getenv("NEOREAD_WEB_PHASE_FIXTURE"); mode != "" {
				phase := "commentary"
				if mode == "legacy" {
					phase = ""
				}
				emit("item/started", map[string]any{"id": "before-search", "type": "agentMessage", "phase": phase})
				encoder.Encode(map[string]any{"method": "item/agentMessage/delta", "params": map[string]string{"threadId": "thread", "turnId": "turn", "itemId": "before-search", "delta": "PRE_SEARCH_COMMENTARY"}})
				emit("item/completed", map[string]any{"id": "before-search", "type": "agentMessage", "phase": phase, "text": "PRE_SEARCH_COMMENTARY"})
			}
			if web == "live" {
				emit("item/started", map[string]any{"id": "search", "type": "webSearch", "query": "official docs", "action": map[string]string{"type": "search", "query": "official docs"}})
				emit("item/completed", map[string]any{"id": "search", "type": "webSearch", "query": "official docs", "results": []any{map[string]string{"url": "https://example.com/docs", "title": "Official docs", "snippet": "A public source"}}})
				if os.Getenv("NEOREAD_WEB_BUDGET_FIXTURE") == "1" {
					if !scanner.Scan() {
						os.Exit(5)
					}
					var steer codexRPCMessage
					if json.Unmarshal(scanner.Bytes(), &steer) != nil || steer.Method != "turn/steer" || !strings.Contains(string(steer.Params), "预算") || !strings.Contains(string(steer.Params), "尽快") {
						os.Exit(6)
					}
					encoder.Encode(map[string]any{"id": steer.ID, "result": map[string]any{}})
				}
				emit("item/started", map[string]any{"id": "read", "type": "webSearch", "query": "", "action": map[string]string{"type": "openPage", "url": "https://example.com/docs"}})
				emit("item/completed", map[string]any{"id": "read", "type": "webSearch", "query": "", "action": map[string]string{"type": "openPage", "url": "https://example.com/docs"}})
			}
			if os.Getenv("NEOREAD_WEB_PHASE_FIXTURE") != "legacy" {
				emit("item/started", map[string]any{"id": "answer", "type": "agentMessage", "phase": "final_answer"})
				encoder.Encode(map[string]any{"method": "item/agentMessage/delta", "params": map[string]string{"threadId": "thread", "turnId": "turn", "itemId": "answer", "delta": "See [Official docs](https://example.com/docs)."}})
				emit("item/completed", map[string]any{"id": "answer", "type": "agentMessage", "phase": "final_answer", "text": "See [Official docs](https://example.com/docs)."})
			}
			encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread", "turn": map[string]string{"id": "turn", "status": "completed"}}})
		}
	}
}

func TestCodexWebBudgetSteersAndCompletes(t *testing.T) {
	fakeAgent(t, "codex", "web-events")
	t.Setenv("NEOREAD_WEB_BUDGET_FIXTURE", "1")
	session, err := isolatedDocumentCodex(t.Context(), t.TempDir(), "test", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	session.finalMessagesOnly = true
	chat := &chatInput{WebSearch: true, webCalls: webToolCalls - 11}
	text, err := session.generate(t.Context(), AIInput{Prompt: "question", Chat: chat}, nil)
	if err != nil || !strings.Contains(text, "Official docs") {
		t.Fatal("budget notice should allow final answer", err, text)
	}
}
func TestCodexWebSearchIPCToChatHistory(t *testing.T) {
	fakeAgent(t, "codex", "web-events")
	s := testServer(t)
	_, err := s.Store.DB.Exec("INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at) VALUES('doc','pdf','test','',1,?,?)", now(), now())
	if err != nil {
		t.Fatal(err)
	}
	config := s.aiConfig()
	config.WebSearch = &WebSearchConfig{CLIEnabled: true}
	if err := s.writeAIConfig(config); err != nil {
		t.Fatal(err)
	}
	body := `{"provider":"codex","prompt":"Look up official docs","context":""}`
	out := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(body))
	if out.Code != 200 || !strings.Contains(out.Body.String(), "event: done") || !strings.Contains(out.Body.String(), "event: tool") || !strings.Contains(out.Body.String(), "读取网页") {
		t.Fatal(out.Code, out.Body.String())
	}
	history, err := s.Store.chatHistory("doc")
	if err != nil || len(history) != 2 || !history[0].WebSearch || len(history[1].Tools) != 2 || history[1].Tools[0].Status != "completed" || len(history[1].WebSources) != 1 {
		t.Fatal(history, err)
	}
	config.WebSearch.CLIEnabled = false
	if err := s.writeAIConfig(config); err != nil {
		t.Fatal(err)
	}
	denied := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(`{"provider":"codex","prompt":"bypass settings","context":"","webSearch":true}`))
	if denied.Code != 400 {
		t.Fatal("request bypassed disabled setting", denied.Code)
	}
	history, err = s.Store.chatHistory("doc")
	if err != nil || len(history) != 2 {
		t.Fatal("rejected request changed saved history")
	}
}
func TestCodexOfflineIPCDoesNotExposeSearch(t *testing.T) {
	fakeAgent(t, "codex", "web-events")
	s := testServer(t)
	chat := &chatInput{WebSearch: false}
	_, err := invokeCLI(t.Context(), s.Store.Root, "codex", "test", AIInput{Prompt: "Answer without web", Chat: chat}, nil)
	if err != nil || len(chat.Tools) != 0 {
		t.Fatal(err, chat.Tools)
	}
}

// This probe initializes the installed CLI only; it never starts a model turn.
func TestCodexWebPreflightNative(t *testing.T) {
	if os.Getenv("NEOREAD_TEST_CODEX_WEB_PREFLIGHT") != "1" {
		t.Skip("opt-in native CLI configuration check")
	}
	for _, enabled := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		session, err := isolatedDocumentCodex(ctx, t.TempDir(), "", "", "", enabled)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		servers, err := session.verifyDocumentConfig(ctx)
		for _, s := range servers {
			if s.Enabled == nil || *s.Enabled {
				t.Fatal("unrelated MCP remained enabled")
			}
		}
		session.close()
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodexWebChatDropsCommentary(t *testing.T) {
	fakeAgent(t, "codex", "web-events")
	t.Setenv("NEOREAD_WEB_PHASE_FIXTURE", "commentary")
	s := testServer(t)
	text, err := invokeCLI(t.Context(), s.Store.Root, "codex", "test", AIInput{Prompt: "question", Chat: &chatInput{WebSearch: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "PRE_SEARCH_COMMENTARY") {
		t.Fatalf("commentary was saved as answer: %q", text)
	}
}
func TestCodexWebSearchInvalidatesOldCandidate(t *testing.T) {
	fakeAgent(t, "codex", "web-events")
	t.Setenv("NEOREAD_WEB_PHASE_FIXTURE", "legacy")
	session, err := isolatedDocumentCodex(t.Context(), t.TempDir(), "test", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	session.finalMessagesOnly = true
	text, err := session.generate(t.Context(), AIInput{Prompt: "question", Chat: &chatInput{WebSearch: true}}, nil)
	if err == nil {
		t.Fatalf("search-before candidate accepted without a final answer: %q", text)
	}
}
