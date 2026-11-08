import { createInterface } from "node:readline";
import { runAgent, type AgentRequest, type ToolResult } from "./runtime";
import { AgentFailure } from "./failure";
import { ModelTransport } from "./transport";

const lines = createInterface({ input: process.stdin, crlfDelay: Infinity });
const controller = new AbortController();
const pending = new Map<
  string,
  { resolve: (value: ToolResult) => void; reject: (error: Error) => void }
>();
let started = false,
  runId = "",
  counter = 0;
const send = (event: Record<string, unknown>) =>
  process.stdout.write(JSON.stringify({ version: 1, runId, ...event }) + "\n");
const close = () => {
  controller.abort();
  for (const entry of pending.values()) entry.reject(new Error("运行已取消"));
  pending.clear();
};
process.on("SIGINT", close);
process.on("SIGTERM", close);
lines.on("close", close);
lines.on("line", (line) => {
  try {
    if (Buffer.byteLength(line) > 64 * 1024 * 1024) throw new Error("请求过大");
    const message = JSON.parse(line);
    if (message.type === "start" && !started) {
      started = true;
      const input = message.input as AgentRequest;
      runId = input.runId;
      let currentMessageVisible = false;
      const emit = (event: Record<string, unknown>) => {
        if (event.type === "draft" && event.reset)
          currentMessageVisible = false;
        if (
          (event.type === "draft" || event.type === "reasoning") &&
          event.text
        )
          currentMessageVisible = true;
        send(event);
      };
      const transport = new ModelTransport(
        input,
        globalThis.fetch,
        emit,
        controller.signal,
        () => currentMessageVisible,
      );
      globalThis.fetch = transport.fetch;
      void runAgent(
        input,
        emit,
        async (name, args, signal) => {
          signal?.throwIfAborted();
          const id = String(++counter);
          return new Promise<ToolResult>((resolve, reject) => {
            pending.set(id, { resolve, reject });
            send({ type: "tool", id, name, args });
          });
        },
        controller.signal,
      )
        .then((result) => {
          send({ type: "complete", ...result, attempts: transport.attempts });
        })
        .catch((error: unknown) => {
          if (controller.signal.aborted) return;
          const failure =
            transport.failure ??
            (error instanceof AgentFailure
              ? error
              : new AgentFailure(
                  transport.phase === "stream"
                    ? "stream_interrupted"
                    : "stop_other",
                  transport.phase,
                ));
          send({
            type: "error",
            code: failure.code,
            status: failure.status ?? transport.status,
            phase: failure.phase,
            attempts: transport.attempts,
            message: failure.message,
          });
          process.exitCode = 1;
        })
        .finally(() => {
          lines.close();
          process.stdin.destroy();
        });
    } else if (message.type === "toolResult" && message.runId === runId) {
      const entry = pending.get(message.id);
      if (!entry) throw new Error("工具结果编号不匹配");
      pending.delete(message.id);
      entry.resolve({ content: message.content, details: undefined });
    } else if (message.type === "cancel" && message.runId === runId) close();
    else throw new Error("不兼容的 Agent 消息");
  } catch {
    send({
      type: "error",
      code: "invalid_message",
      phase: "validation",
      message: "Agent 消息格式无效",
    });
    process.exitCode = 1;
    close();
    lines.close();
    process.stdin.destroy();
  }
});
