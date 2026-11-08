import {
  createAgentSession,
  createExtensionRuntime,
  ModelRuntime,
  SessionManager,
  SettingsManager,
  type ResourceLoader,
  type ToolDefinition,
  type FileEntry,
} from "@earendil-works/pi-coding-agent";
import {
  createProvider,
  InMemoryCredentialStore,
  InMemoryModelsStore,
  type Api,
  type Model,
  type ImageContent,
} from "@earendil-works/pi-ai";
import { openAICompletionsApi } from "@earendil-works/pi-ai/api/openai-completions.lazy";
import { anthropicMessagesApi } from "@earendil-works/pi-ai/api/anthropic-messages.lazy";
import { Type } from "typebox";
import { AgentFailure, type FailureCode } from "./failure";

export interface AgentRequest {
  version: 1;
  runId: string;
  prompt: string;
  system: string;
  model: Model<Api>;
  key: string;
  payload?: Record<string, unknown>;
  endpoint?: string;
  effort: "off" | "minimal" | "low" | "medium" | "high" | "xhigh" | "max";
  entries?: FileEntry[];
  images?: ImageContent[];
  tools: {
    name: string;
    description: string;
    schema: Record<string, unknown>;
  }[];
}
export type ToolResult = {
  content: ({ type: "text"; text: string } | ImageContent)[];
  details: undefined;
};
export type AgentEvent = { type: string; [key: string]: unknown };

/** The host owns the accepted entry tree. No user configuration or files are discovered. */
export async function runAgent(
  input: AgentRequest,
  emit: (event: AgentEvent) => void,
  callTool: (
    name: string,
    args: unknown,
    signal?: AbortSignal,
  ) => Promise<ToolResult>,
  signal: AbortSignal,
) {
  if (input.version !== 1 || !input.runId || !input.model || !input.prompt)
    throw new AgentFailure("invalid_message", "validation");
  const model = input.model;
  if (!["openai-completions", "anthropic-messages"].includes(model.api))
    throw new AgentFailure("invalid_message", "validation");
  const modelRuntime = await ModelRuntime.create({
    credentials: new InMemoryCredentialStore(),
    modelsStore: new InMemoryModelsStore(),
    modelsPath: null,
    refreshOnCreate: false,
    allowModelNetwork: false,
    signal,
  });
  const provider = createProvider({
    id: model.provider,
    name: "NeoRead",
    baseUrl: model.baseUrl,
    models: [model],
    auth: {
      apiKey: {
        name: "NeoRead API",
        resolve: async () => ({
          auth: { apiKey: input.key || "neoread-keyless" },
        }),
      },
    },
    api:
      model.api === "anthropic-messages"
        ? anthropicMessagesApi()
        : openAICompletionsApi(),
  });
  modelRuntime.registerNativeProvider(provider);
  await modelRuntime.setRuntimeApiKey(
    model.provider,
    input.key || "neoread-keyless",
  );
  const resourceLoader: ResourceLoader = {
    getExtensions: () => ({
      extensions: [],
      errors: [],
      runtime: createExtensionRuntime(),
    }),
    getSkills: () => ({ skills: [], diagnostics: [] }),
    getPrompts: () => ({ prompts: [], diagnostics: [] }),
    getThemes: () => ({ themes: [], diagnostics: [] }),
    getAgentsFiles: () => ({ agentsFiles: [] }),
    getSystemPrompt: () => input.system,
    getSystemPromptSource: () => undefined,
    getAppendSystemPrompt: () => [],
    getAppendSystemPromptSources: () => [],
    extendResources: () => {},
    reload: async () => {},
  };
  const tools: ToolDefinition[] = input.tools.map((tool) => ({
    name: tool.name,
    label: tool.name,
    description: tool.description,
    parameters: Type.Unsafe(tool.schema),
    execute: async (_id, args, abort) => callTool(tool.name, args, abort),
  }));
  const manager = SessionManager.inMemory(process.cwd(), {}, input.entries);
  const { session } = await createAgentSession({
    cwd: process.cwd(),
    agentDir: process.cwd(),
    model,
    modelRuntime,
    resourceLoader,
    sessionManager: manager,
    thinkingLevel: input.effort,
    settingsManager: SettingsManager.inMemory({
      // pi retries whole assistant turns, including errors after streamed text.
      // Only ModelTransport may retry before a response is handed to pi.
      retry: { enabled: false, provider: { maxRetries: 0 } },
      cacheWarming: "off",
      compaction: {
        enabled: true,
        reserveTokens: Math.min(8192, Math.floor(model.contextWindow / 4)),
        keepRecentTokens: Math.min(12000, Math.floor(model.contextWindow / 3)),
      },
    }),
    tools: tools.map((tool) => tool.name),
    customTools: tools,
    noTools: "builtin",
  });
  const maxToolRounds = 255;
  let rounds = 0,
    outputBytes = 0,
    reasoningBytes = 0,
    draftBytes = 0;
  let failure: AgentFailure | undefined;
  const stop = () => {
    void session.abort();
  };
  signal.addEventListener("abort", stop, { once: true });
  if (signal.aborted) stop();
  const unsubscribe = session.subscribe((event) => {
    if (event.type === "turn_start") ++rounds;
    if (
      event.type === "turn_end" &&
      rounds === maxToolRounds &&
      event.message.role === "assistant" &&
      event.message.content.some((part) => part.type === "toolCall")
    ) {
      session.agent.steer({
        role: "user",
        content: [
          {
            type: "text",
            text: "本轮工具调用已接近预算，请关注剩余时间和上下文，优先补充必要证据并尽快完成回答。",
          },
        ],
        timestamp: Date.now(),
      });
    }
    if (event.type === "message_start" && event.message.role === "assistant") {
      draftBytes = 0;
      emit({ type: "draft", reset: true, text: "" });
    }
    if (event.type === "message_update") {
      const delta = event.assistantMessageEvent;
      if (delta.type === "text_delta" || delta.type === "thinking_delta") {
        const bytes = Buffer.byteLength(delta.delta);
        outputBytes += bytes;
        if (delta.type === "thinking_delta") reasoningBytes += bytes;
        else draftBytes += bytes;
        if (
          outputBytes > 4 * 1024 * 1024 ||
          reasoningBytes > 1024 * 1024 ||
          draftBytes > 1024 * 1024
        ) {
          failure = new AgentFailure("output_limit", "stream");
          stop();
          return;
        }
        emit({
          type: delta.type === "text_delta" ? "draft" : "reasoning",
          text: delta.delta,
        });
      }
    }
    if (event.type === "message_end" && event.message.role === "assistant") {
      const m = event.message;
      if (
        !signal.aborted &&
        !failure &&
        m.stopReason !== "stop" &&
        m.stopReason !== "toolUse"
      ) {
        let code: FailureCode = "stop_other";
        if (m.stopReason === "length") code = "stop_length";
        else if (
          m.stopReason === "error" &&
          (!m.errorMessage?.startsWith("Provider finish_reason:") ||
            m.errorMessage === "Provider finish_reason: network_error")
        )
          code = "stream_interrupted";
        failure = new AgentFailure(code, "stream");
        // Stop post-turn compaction recovery from replaying a failed answer.
        stop();
      }
      emit({ type: "usage", model: m.model, usage: m.usage });
      if (m.content.some((part) => part.type === "toolCall"))
        emit({ type: "draft", reset: true, text: "" });
    }
    if (event.type === "compaction_end" && event.result?.usage) {
      emit({ type: "usage", model: model.id, usage: event.result.usage });
    }
    if (event.type === "compaction_start")
      emit({ type: "activity", message: "正在整理对话上下文" });
  });
  try {
    signal.throwIfAborted();
    await session.prompt(input.prompt, {
      images: input.images,
      expandPromptTemplates: false,
    });
    await session.waitForIdle();
    signal.throwIfAborted();
    if (failure) throw failure;
    const last = session.messages.at(-1);
    if (!last || last.role !== "assistant" || last.stopReason !== "stop")
      throw new AgentFailure("stop_other", "validation");
    const text = last.content
      .filter((part) => part.type === "text")
      .map((part) => part.text)
      .join("");
    if (!text.trim()) throw new AgentFailure("empty_answer", "validation");
    // Images stay in NeoRead's document cache. Historical attachments must be
    // re-read through scoped tools, never silently become unbounded DB blobs.
    const entries = JSON.parse(
      JSON.stringify(
        [manager.getHeader(), ...manager.getEntries()],
        (_key, value: unknown) => {
          if (
            value &&
            typeof value === "object" &&
            "type" in value &&
            value.type === "image"
          )
            return {
              type: "text",
              text: "[历史图片：需要时重新调用 read_pdf_pages 核对原图；手动附件需重新提供。]",
            };
          return value;
        },
      ),
    ) as FileEntry[];
    if (Buffer.byteLength(JSON.stringify(entries)) > 32 * 1024 * 1024)
      throw new AgentFailure("output_limit", "validation");
    return { text, entries };
  } catch (error) {
    throw failure ?? error;
  } finally {
    signal.removeEventListener("abort", stop);
    unsubscribe();
    session.dispose();
  }
}
