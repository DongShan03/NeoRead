package neoread

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodeAgentModels(t *testing.T) {
	for _, test := range []struct{ provider, raw, id string }{
		{"codex", `{"data":[{"model":"hidden","hidden":true},{"model":"gpt-model","displayName":"GPT Model","isDefault":true}]}`, "gpt-model"},
		{"claude", `{"models":[{"value":"default","resolvedModel":"claude-test"},{"value":"sonnet","resolvedModel":"claude-test","displayName":"Sonnet"}]}`, "claude-test"},
		{"kimi", `{"configOptions":[{"id":"model","category":"model","currentValue":"kimi-test","options":[{"value":"kimi-test","name":"Kimi"}]}]}`, "kimi-test"},
		{"kimi", `{"models":{"currentModelId":"kimi-old","availableModels":[{"modelId":"kimi-old","name":"Kimi"}]}}`, "kimi-old"},
	} {
		t.Run(test.provider+test.id, func(t *testing.T) {
			models, err := decodeAgentModels(test.provider, json.RawMessage(test.raw))
			if err != nil || len(models) != 1 || models[0].ID != test.id || !models[0].Default {
				t.Fatalf("unexpected models: %+v, %v", models, err)
			}
			if test.provider == "claude" && models[0].Aliases[0] != "sonnet" {
				t.Fatal("lost configured alias")
			}
		})
	}
	if _, err := decodeAgentModels("kimi", json.RawMessage(`{}`)); err == nil {
		t.Fatal("empty catalog must report failure")
	}
}

func TestDiscoverModelsProtocol(t *testing.T) {
	// The fake CLI rejects generation requests and verifies its isolated cwd.
	for _, provider := range []string{"codex", "claude", "kimi"} {
		t.Run(provider, func(t *testing.T) {
			s := testServer(t)
			fakeAgent(t, provider, "models")
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			models, err := discoverModels(ctx, s.Store.Root, provider)
			if err != nil || len(models) != 1 || models[0].ID != "model-a" {
				t.Fatalf("models %+v: %v", models, err)
			}
			files, err := os.ReadDir(filepath.Join(s.Store.Root, "ai-work"))
			if err != nil || len(files) != 0 {
				t.Fatalf("discovery workdir leaked: %v", err)
			}
			response := request(t, s, "GET", "/api/ai/models?provider="+provider, nil)
			if response.Code != 200 || !strings.Contains(response.Body.String(), "model-a") {
				t.Fatalf("HTTP discovery failed: %s", response.Body)
			}
		})
	}
}

func TestModelDiscoveryRejectsUnknownProviderAndCancels(t *testing.T) {
	s := testServer(t)
	if r := request(t, s, "GET", "/api/ai/models?provider=invalid", nil); r.Code != 400 {
		t.Fatal(r.Code)
	}
	fakeAgent(t, "codex", "models-wait")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := discoverModels(ctx, s.Store.Root, "codex"); err == nil {
		t.Fatal("expected cancellation")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("CLI did not stop after cancellation")
	}
}

func catalog(ids ...string) []AgentModel {
	models := []AgentModel{}
	for i, id := range ids {
		models = append(models, AgentModel{ID: id, Name: id, Default: i == 0})
	}
	return models
}

func TestDefaultTaskModelPerAgent(t *testing.T) {
	codex := catalog("gpt-6.1-sol", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna")
	claude := catalog("claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1", "claude-haiku-4-5-20251001", "claude-sonnet-5", "claude-opus-5", "claude-fable-5", "claude-opus-4-8", "claude-sonnet-4-6")
	claude[1].Aliases = []string{"sonnet"}
	kimi := catalog("moonshot-cn/kimi-k3", "moonshot-cn/kimi-k2.7-code")
	for _, tt := range []struct {
		name     string
		task     modelTask
		provider string
		models   []AgentModel
		want     string
	}{
		{"codex chat takes the newest sol, never astra", taskChat, "codex", codex, "gpt-6.1-sol"},
		{"codex translation takes the newest luna", taskTranslation, "codex", codex, "gpt-6-luna"},
		{"claude chat takes the newest opus, never fable", taskChat, "claude", claude, "claude-opus-5-5"},
		{"claude translation takes the newest sonnet", taskTranslation, "claude", claude, "claude-sonnet-5-5"},
		{"claude translation falls back to haiku", taskTranslation, "claude", catalog("claude-opus-5-5", "claude-haiku-4-5-20251001", "claude-haiku-3-5"), "claude-haiku-4-5-20251001"},
		{"kimi keeps its CLI default", taskChat, "kimi", kimi, ""},
		{"kimi has no fast tier", taskTranslation, "kimi", kimi, ""},
		{"kimi flash tier", taskTranslation, "kimi", catalog("kimi-k3", "kimi-k3-flash"), "kimi-k3-flash"},
		{"general beats code variant", taskTranslation, "codex", catalog("gpt-7-codex-mini", "gpt-6-mini"), "gpt-6-mini"},
		{"whole tokens only", taskTranslation, "codex", catalog("gemini-pro", "lunar-x"), ""},
		{"empty catalog", taskChat, "claude", nil, ""},
		{"6.1 beats 6", taskChat, "codex", catalog("gpt-6-sol", "gpt-6.1-sol", "gpt-5.6-sol"), "gpt-6.1-sol"},
		{"6.10 beats 6.9", taskChat, "codex", catalog("gpt-6.9-sol", "gpt-6.10-sol"), "gpt-6.10-sol"},
		{"6 beats 5.6", taskTranslation, "codex", catalog("gpt-5.6-luna", "gpt-6-luna"), "gpt-6-luna"},
		{"date snapshot is not a version", taskChat, "codex", catalog("gpt-6-sol-2026-01-15", "gpt-6.1-sol"), "gpt-6.1-sol"},
		{"claude dated id", taskTranslation, "claude", catalog("claude-haiku-4-5-20251001", "claude-haiku-4-6"), "claude-haiku-4-6"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := defaultTaskModel(tt.task, tt.provider, tt.models); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestTaskRecommendationsMarkCatalog(t *testing.T) {
	models := taskRecommendations("claude", catalog("claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1"))
	got := map[string]string{}
	for _, m := range models {
		got[m.ID] = strings.Join(m.RecommendedFor, ",")
	}
	if got["claude-opus-5-5"] != "chat" || got["claude-sonnet-5-5"] != "translation" || got["claude-fable-5-1"] != "" {
		t.Fatal(got)
	}
}

func TestTranslationModelAndEffortAreIndependent(t *testing.T) {
	s := testServer(t)
	c := AIConfig{Primary: "claude", Models: map[string]string{"claude": "chat-model"}, Efforts: map[string]map[string]string{"claude": {"chat-model": "max"}},
		TranslationModels: map[string]string{"claude": "fast-model"}, TranslationEfforts: map[string]map[string]string{"claude": {"fast-model": "low"}}}
	if r := request(t, s, "PUT", "/api/ai/config", configJSON(c)); r.Code != 200 {
		t.Fatal(r.Body)
	}
	// An older client that omits translation fields must not erase them.
	c.TranslationModels, c.TranslationEfforts = nil, nil
	if r := request(t, s, "PUT", "/api/ai/config", configJSON(c)); r.Code != 200 {
		t.Fatal(r.Body)
	}
	saved := s.aiConfig()
	if saved.TranslationModels["claude"] != "fast-model" || saved.TranslationEfforts["claude"]["fast-model"] != "low" || saved.Efforts["claude"]["chat-model"] != "max" {
		t.Fatalf("%+v", saved)
	}
	c.TranslationEfforts = map[string]map[string]string{"claude": {"fast-model": "ultra"}}
	if r := request(t, s, "PUT", "/api/ai/config", configJSON(c)); r.Code != 400 {
		t.Fatal(r.Body)
	}
}

func TestUnsetModelsResolvePerTask(t *testing.T) {
	efforts := []string{"low", "medium", "high", "max"}
	for _, tt := range []struct {
		task       modelTask
		model, eff string
	}{
		{taskChat, "claude-opus-5-5", "medium"},
		{taskTranslation, "claude-sonnet-5-5", "medium"},
	} {
		t.Run(string(tt.task), func(t *testing.T) {
			fakeAgent(t, "claude", "complete")
			capture := filepath.Join(t.TempDir(), "request.json")
			t.Setenv("NEOREAD_AGENT_CAPTURE", capture)
			s, _ := imageFixture(t)
			models := catalog("claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1")
			for i := range models {
				models[i].SupportedEfforts = efforts
			}
			s.modelCache = map[string]modelCatalogEntry{"claude": {models: models, expires: time.Now().Add(time.Minute)}}
			c := AIConfig{Primary: "claude", Models: map[string]string{}, Capabilities: map[string]Capability{}}
			c.Capabilities["claude"] = Capability{Text: true, Fingerprint: configPrint(c, "claude")}
			if _, err := s.taskGenerationService(c, tt.task).Generate(t.Context(), AIInput{Prompt: "test"}, false, nil); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), "--model "+tt.model) || !strings.Contains(string(b), "--effort "+tt.eff) {
				t.Fatal(string(b))
			}
		})
	}
}

func TestTranslationFailureKeepsChatCapability(t *testing.T) {
	s := testServer(t)
	c := AIConfig{Primary: "claude", Models: map[string]string{}, Capabilities: map[string]Capability{}}
	c.Capabilities["claude"] = Capability{Text: true, CheckedAt: "t", Fingerprint: configPrint(c, "claude")}
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	for _, task := range []modelTask{taskTranslation, taskChat} {
		service := s.taskGenerationService(c, task)
		connection := service.connections["claude"]
		connection.Adapter = adapterFunc(func(context.Context, GenerateRequest, func(ProviderEvent) error) (GenerateResult, error) {
			return GenerateResult{}, errors.New("unknown model")
		})
		service.connections["claude"] = connection
		_, _ = service.Generate(t.Context(), AIInput{Prompt: "test"}, false, nil)
		if !capable(s.aiConfig(), "claude", false) {
			t.Fatalf("%s runtime failure disabled verified chat capability", task)
		}
	}
}

func TestModelVersion(t *testing.T) {
	for id, want := range map[string]string{
		"gpt-6.1-sol":                "[6 1]",
		"gpt-6-sol-2026-01-15":       "[6]",
		"claude-haiku-4-5-20251001":  "[4 5]",
		"claude-3-5-sonnet-20241022": "[3 5]",
		"moonshot-cn/kimi-k2.7-code": "[2 7]",
	} {
		if got := fmt.Sprint(modelVersion(id)); got != want {
			t.Fatalf("%s: got %s want %s", id, got, want)
		}
	}
}

func TestAutomaticModelFallsBackToCLIDefault(t *testing.T) {
	models := catalog("claude-opus-5-5", "claude-sonnet-5-5")
	for i := range models {
		models[i].SupportedEfforts = []string{"low", "medium", "high"}
	}
	adapter := func(model string) configuredCLIAdapter {
		return configuredCLIAdapter{cliAdapter{"", "claude", model}, "medium", taskTranslation,
			func(context.Context, string) ([]AgentModel, error) { return models, nil }}
	}
	t.Run("occupied thread does not retry another model", func(t *testing.T) {
		calls := 0
		busy := generationError(ErrorBusy, "会话仍由 Codex App 占用")
		_, err := adapter("").streamWith(t.Context(), GenerateRequest{}, nil, func(cliAdapter, GenerateRequest, func(ProviderEvent) error) (GenerateResult, error) {
			calls++
			return GenerateResult{}, busy
		})
		if calls != 1 || !errors.Is(err, busy) {
			t.Fatal("occupied thread retried with another model", calls, err)
		}
	})
	for _, tt := range []struct {
		name       string
		configured string
		partial    bool
		emitErr    error
		want       []string
		ok         bool
	}{
		{"automatic choice retries with the CLI default", "", false, nil, []string{"claude-sonnet-5-5", ""}, true},
		{"user choice is never replaced", "claude-sonnet-5-5", false, nil, []string{"claude-sonnet-5-5"}, false},
		{"visible text is never followed by a retry", "", true, nil, []string{"claude-sonnet-5-5"}, false},
		{"consumer failure is not retried", "", false, errors.New("save"), []string{"claude-sonnet-5-5"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls, efforts []string
			emit := func(ProviderEvent) error { return tt.emitErr }
			_, err := adapter(tt.configured).streamWith(t.Context(), GenerateRequest{}, emit, func(cli cliAdapter, req GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
				calls = append(calls, cli.model)
				efforts = append(efforts, req.Input.Effort)
				if cli.model == "" {
					return GenerateResult{Text: "ok", FinishReason: "stop"}, nil
				}
				if tt.partial {
					_ = emit(ProviderEvent{Text: "部分"})
				}
				if tt.emitErr != nil {
					return GenerateResult{}, emit(ProviderEvent{Metrics: []ModelTokens{{Model: "m"}}})
				}
				return GenerateResult{}, errors.New("model not available")
			})
			if fmt.Sprint(calls) != fmt.Sprint(tt.want) || (err == nil) != tt.ok {
				t.Fatalf("calls %q err %v", calls, err)
			}
			for _, effort := range efforts {
				if effort != "medium" {
					t.Fatalf("effort %q", effort)
				}
			}
		})
	}
}

func TestNativeEffortMapping(t *testing.T) {
	for _, tt := range []struct {
		name, provider  string
		supported, want []string
	}{
		{"codex extended", "codex", []string{"low", "medium", "high", "xhigh", "max", "ultra"}, []string{"low", "medium", "high", "max"}},
		{"codex older", "codex", []string{"low", "medium", "high", "xhigh"}, []string{"low", "medium", "high", "xhigh"}},
		{"claude", "claude", []string{"low", "medium", "high", "max"}, []string{"low", "medium", "high", "max"}},
		{"haiku", "claude", nil, []string{"", "", "", ""}},
		{"kimi k3", "kimi", []string{"low", "high", "max"}, []string{"low", "high", "high", "max"}},
		{"kimi k2", "kimi", []string{"off", "on", "high"}, []string{"off", "on", "high", "high"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			models := []AgentModel{{ID: "model", Default: true, Aliases: []string{"alias"}, SupportedEfforts: tt.supported}}
			for i, level := range []string{"low", "medium", "high", "max"} {
				for _, model := range []string{"model", "alias", ""} {
					if got := nativeEffort(tt.provider, level, model, models); got != tt.want[i] {
						t.Fatalf("%s/%s: got %q want %q", model, level, got, tt.want[i])
					}
				}
			}
		})
	}
}

func TestEffortPersistsAndRejectsNativeOnlyValues(t *testing.T) {
	s := testServer(t)
	c := AIConfig{Primary: "codex", Models: map[string]string{"codex": "model"}, Efforts: map[string]map[string]string{"codex": {"model": "max"}}}
	if r := request(t, s, "PUT", "/api/ai/config", configJSON(c)); r.Code != 200 {
		t.Fatal(r.Body)
	}
	if got := s.aiConfig().Efforts["codex"]["model"]; got != "max" {
		t.Fatal(got)
	}
	// An older settings client omitting the optional field must not erase it.
	c.Efforts = nil
	if r := request(t, s, "PUT", "/api/ai/config", configJSON(c)); r.Code != 200 {
		t.Fatal(r.Body)
	}
	if got := s.aiConfig().Efforts["codex"]["model"]; got != "max" {
		t.Fatal("effort lost", got)
	}
	for _, value := range []string{"xhigh", "ultra", "on", "invalid"} {
		c.Efforts = map[string]map[string]string{"codex": {"model": value}}
		if r := request(t, s, "PUT", "/api/ai/config", configJSON(c)); r.Code != 400 {
			t.Fatalf("accepted %s: %s", value, r.Body)
		}
	}
}

func TestConfiguredEffortReachesProvider(t *testing.T) {
	for _, tt := range []struct {
		provider, level, native string
		supported               []string
	}{
		{"codex", "max", "xhigh", []string{"low", "medium", "high", "xhigh"}},
		{"claude", "high", "high", []string{"low", "medium", "high"}},
		{"kimi", "medium", "on", []string{"off", "on", "high"}},
	} {
		for _, route := range []string{"invoke", "background", "chat"} {
			t.Run(tt.provider+"/"+route, func(t *testing.T) {
				fakeAgent(t, tt.provider, "complete")
				capture := filepath.Join(t.TempDir(), "request.json")
				t.Setenv("NEOREAD_AGENT_CAPTURE", capture)
				s, _ := imageFixture(t)
				s.modelCache = map[string]modelCatalogEntry{tt.provider: {models: []AgentModel{{ID: "model", SupportedEfforts: tt.supported}}, expires: time.Now().Add(time.Minute)}}
				c := AIConfig{Primary: tt.provider, Capabilities: map[string]Capability{}, Models: map[string]string{tt.provider: "model"}, Efforts: map[string]map[string]string{tt.provider: {"model": tt.level}}}
				c.Capabilities[tt.provider] = Capability{Text: true, Fingerprint: configPrint(c, tt.provider)}
				if route == "chat" {
					if err := s.writeAIConfig(c); err != nil {
						t.Fatal(err)
					}
					body, _ := json.Marshal(map[string]string{"provider": tt.provider, "prompt": "test", "context": ""})
					res := request(t, s, "POST", "/api/documents/doc/chat", bytes.NewReader(body))
					if res.Code != 200 || !strings.Contains(res.Body.String(), "event: done") || strings.Contains(res.Body.String(), "event: error") {
						t.Fatal(res.Body)
					}
				} else {
					var answer string
					var err error
					if route == "background" {
						result, e := s.generateWithConfig(t.Context(), c, AIInput{Prompt: "test"}, nil, nil)
						answer, err = result.Text, e
					} else {
						answer, err = s.invoke(t.Context(), c, tt.provider, AIInput{Prompt: "test"}, nil)
					}
					if err != nil || answer != "你好，世界" {
						t.Fatalf("%s %v", answer, err)
					}
				}
				b, err := os.ReadFile(capture)
				if err != nil {
					t.Fatal(err)
				}
				if tt.provider == "claude" {
					if !strings.Contains(string(b), "--effort "+tt.native) {
						t.Fatal(string(b))
					}
					return
				}
				var params map[string]any
				if err = json.Unmarshal(b, &params); err != nil {
					t.Fatal(err)
				}
				if tt.provider == "codex" && params["effort"] != tt.native {
					t.Fatal(string(b))
				}
				if tt.provider == "kimi" && (params["configId"] != "thinking" || params["value"] != tt.native || params["sessionId"] != "session") {
					t.Fatal(string(b))
				}
			})
		}
	}
}

func TestDecodeEffortCapabilities(t *testing.T) {
	for _, tt := range []struct{ provider, raw string }{
		{"codex", `{"data":[{"model":"test","defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"high"}]}]}`},
		{"claude", `{"models":[{"value":"test","supportsEffort":true,"supportedEffortLevels":["low","high"]}]}`},
	} {
		models, err := decodeAgentModels(tt.provider, json.RawMessage(tt.raw))
		if err != nil || len(models) != 1 || len(models[0].SupportedEfforts) != 2 {
			t.Fatalf("%+v %v", models, err)
		}
	}
	values, current := kimiEfforts(json.RawMessage(`{"configOptions":[{"id":"thinking","currentValue":"high","options":[{"value":"low"},{"value":"high"},{"value":"max"}]}]}`))
	if len(values) != 3 || current != "high" {
		t.Fatalf("%v %s", values, current)
	}
}

func configJSON(c AIConfig) *bytes.Reader { b, _ := json.Marshal(c); return bytes.NewReader(b) }
