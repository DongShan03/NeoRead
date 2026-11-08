import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { AgentEvent, AgentRequest } from "./runtime";
import { ModelTransport } from "./transport";

const url = "http://127.0.0.1:12345/v1/chat/completions";
const input = {
  model: { baseUrl: "http://127.0.0.1:12345/v1" },
  key: "test-only-key",
} as AgentRequest;

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(Math, "random").mockReturnValue(0.5);
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

function transport(fetch: typeof globalThis.fetch, visible = () => false) {
  const controller = new AbortController();
  const events: AgentEvent[] = [];
  const worker = new ModelTransport(
    input,
    fetch,
    (e) => events.push(e),
    controller.signal,
    visible,
  );
  return { worker, events, controller };
}

test.each([408, 429, 500, 502, 503, 504, 529])(
  "retries HTTP %s before returning a successful response",
  async (status) => {
    const fetch = vi
      .fn<typeof globalThis.fetch>()
      .mockResolvedValueOnce(new Response("PRIVATE_BODY", { status }))
      .mockResolvedValueOnce(new Response("answer"));
    const { worker, events } = transport(fetch);
    const result = worker.fetch(url);
    await vi.advanceTimersByTimeAsync(999);
    expect(fetch).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(await (await result).text()).toBe("answer");
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(events).toEqual([
      { type: "retry", attempt: 1, max: 3, status, delayMs: 1000, attempts: 1 },
    ]);
    expect(worker.failure).toBeUndefined();
  },
);

test.each(["6", new Date(6000).toUTCString(), "999"])(
  "respects Retry-After %s with a 20s cap",
  async (header) => {
    vi.setSystemTime(0);
    const fetch = vi
      .fn<typeof globalThis.fetch>()
      .mockResolvedValueOnce(
        new Response(null, { status: 429, headers: { "Retry-After": header } }),
      )
      .mockResolvedValueOnce(new Response("ok"));
    const { worker, events } = transport(fetch);
    const result = worker.fetch(url);
    const delay = header === "999" ? 20000 : 6000;
    await vi.advanceTimersByTimeAsync(delay - 1);
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(events[0].delayMs).toBe(delay);
    await vi.advanceTimersByTimeAsync(1);
    await result;
    expect(fetch).toHaveBeenCalledTimes(2);
  },
);

test.each([400, 401, 403, 404, 422])(
  "does not retry HTTP %s or expose its body",
  async (status) => {
    const fetch = vi
      .fn<typeof globalThis.fetch>()
      .mockResolvedValue(
        new Response("PRIVATE_BODY test-only-key", { status }),
      );
    const { worker, events } = transport(fetch);
    await expect(worker.fetch(url)).rejects.toMatchObject({
      code: `http_${status}`,
      status,
      phase: "request",
    });
    expect(worker.failure?.message).not.toContain("PRIVATE");
    expect(worker.failure?.message).not.toContain(input.key);
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(events).toEqual([]);
  },
);

test("aborts backoff immediately without another attempt", async () => {
  const fetch = vi
    .fn<typeof globalThis.fetch>()
    .mockResolvedValue(new Response(null, { status: 503 }));
  const { worker, events, controller } = transport(fetch);
  const result = expect(worker.fetch(url)).rejects.toMatchObject({
    name: "AbortError",
  });
  await vi.advanceTimersByTimeAsync(0);
  expect(events).toHaveLength(1);
  controller.abort();
  await result;
  expect(fetch).toHaveBeenCalledTimes(1);
  expect(vi.getTimerCount()).toBe(0);
});

test.each(["http", "network", "timeout"])(
  "exhausts only three retries for %s failures",
  async (mode) => {
    const fetch = vi
      .fn<typeof globalThis.fetch>()
      .mockImplementation(async () => {
        if (mode === "http") return new Response(null, { status: 503 });
        if (mode === "timeout")
          throw new DOMException("PRIVATE", "TimeoutError");
        throw new TypeError("fetch failed PRIVATE", {
          cause: { code: "ECONNRESET" },
        });
      });
    const { worker, events } = transport(fetch);
    const result = expect(worker.fetch(url)).rejects.toMatchObject({
      code: mode === "http" ? "http_5xx" : mode,
      phase: "request",
    });
    await vi.runAllTimersAsync();
    await result;
    expect(fetch).toHaveBeenCalledTimes(4);
    expect(worker.attempts).toBe(4);
    expect(events.map((e) => e.delayMs)).toEqual([1000, 2000, 4000]);
    expect(events.map((e) => e.attempt)).toEqual([1, 2, 3]);
  },
);

test("does not replay a response when its stream fails", async () => {
  const response = new Response(
    new ReadableStream({
      start(controller) {
        controller.enqueue(new TextEncoder().encode("partial"));
        controller.error(new Error("stream dropped"));
      },
    }),
  );
  const fetch = vi.fn<typeof globalThis.fetch>().mockResolvedValue(response);
  const { worker, events } = transport(fetch);
  const result = await worker.fetch(url);
  await expect(result.text()).rejects.toThrow("stream dropped");
  await vi.runAllTimersAsync();
  expect(fetch).toHaveBeenCalledTimes(1);
  expect(events).toEqual([]);
});

test("does not retry a request when the current message already has visible draft", async () => {
  const fetch = vi
    .fn<typeof globalThis.fetch>()
    .mockResolvedValue(new Response(null, { status: 503 }));
  const { worker, events } = transport(fetch, () => true);
  await expect(worker.fetch(url)).rejects.toMatchObject({ code: "http_5xx" });
  expect(fetch).toHaveBeenCalledTimes(1);
  expect(events).toEqual([]);
});

test("preserves payload rewriting, keyless headers, origin and redirect restrictions", async () => {
  const requests: Request[] = [];
  const fetch = vi
    .fn<typeof globalThis.fetch>()
    .mockImplementation(async (request, init) => {
      requests.push(new Request(request, init));
      return new Response("ok");
    });
  const worker = new ModelTransport(
    { ...input, key: "", endpoint: url, payload: { max_tokens: 100 } },
    fetch,
    () => {},
    new AbortController().signal,
    () => false,
  );
  await worker.fetch(url, {
    method: "POST",
    body: JSON.stringify({
      messages: [],
      max_tokens: 5,
      reasoning_effort: "high",
    }),
    headers: { Authorization: "Bearer test", "x-api-key": "test" },
  });
  expect(requests[0].redirect).toBe("error");
  expect(requests[0].headers.has("authorization")).toBe(false);
  expect(requests[0].headers.has("x-api-key")).toBe(false);
  expect(await requests[0].json()).toEqual({ messages: [], max_tokens: 100 });
  await expect(worker.fetch("http://other.invalid/v1")).rejects.toMatchObject({
    code: "invalid_message",
  });
  expect(fetch).toHaveBeenCalledTimes(1);
});

test("replays Request bodies through the configured endpoint", async () => {
  const requests: Request[] = [];
  const fetch = vi
    .fn<typeof globalThis.fetch>()
    .mockImplementation(async (request, init) => {
      requests.push(new Request(request, init));
      return new Response(null, { status: requests.length === 1 ? 503 : 200 });
    });
  const worker = new ModelTransport(
    { ...input, endpoint: url },
    fetch,
    () => {},
    new AbortController().signal,
    () => false,
  );
  const result = worker.fetch(
    new Request(url, { method: "POST", body: "request payload" }),
  );
  await vi.runAllTimersAsync();
  await result;
  expect(requests).toHaveLength(2);
  for (const request of requests) {
    expect(request.method).toBe("POST");
    expect(await request.text()).toBe("request payload");
  }
});
