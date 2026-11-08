import { AgentFailure } from "./failure";
import type { AgentEvent, AgentRequest } from "./runtime";

const retryStatuses = new Set([408, 429, 500, 502, 503, 504, 529]);
const maxRetries = 3;

function waitForRetry(delayMs: number, signal: AbortSignal) {
  signal.throwIfAborted();
  return new Promise<void>((resolve, reject) => {
    const abort = () => {
      clearTimeout(timer);
      reject(signal.reason);
    };
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", abort);
      resolve();
    }, delayMs);
    signal.addEventListener("abort", abort, { once: true });
  });
}

function retryDelay(attempt: number, retryAfter: string | null) {
  const backoff = Math.round(
    1000 * 2 ** (attempt - 1) * (0.75 + Math.random() * 0.5),
  );
  if (retryAfter === null) return backoff;
  const seconds = Number(retryAfter);
  const delay = Number.isFinite(seconds)
    ? seconds * 1000
    : Date.parse(retryAfter) - Date.now();
  return Math.min(20000, Math.max(backoff, Number.isFinite(delay) ? delay : 0));
}

export class ModelTransport {
  attempts = 0;
  failure?: AgentFailure;
  status?: number;
  phase: "request" | "stream" = "request";
  private readonly origin: string;

  constructor(
    private readonly input: AgentRequest,
    private readonly upstreamFetch: typeof globalThis.fetch,
    private readonly emit: (event: AgentEvent) => void,
    private readonly signal: AbortSignal,
    private readonly hasCurrentMessageOutput: () => boolean,
  ) {
    this.origin = new URL(input.model.baseUrl).origin;
    if (input.endpoint && new URL(input.endpoint).origin !== this.origin)
      throw new AgentFailure("invalid_message", "validation");
  }

  fetch: typeof globalThis.fetch = async (url, init) => {
    this.failure = undefined;
    this.status = undefined;
    this.phase = "request";
    const target = new URL(url instanceof Request ? url.url : String(url));
    if (target.origin !== this.origin)
      throw new AgentFailure("invalid_message", "validation");
    const headers = new Headers(
      init?.headers ?? (url instanceof Request ? url.headers : undefined),
    );
    if (!this.input.key) {
      headers.delete("authorization");
      headers.delete("x-api-key");
    }
    let body = init?.body;
    if (this.input.payload && typeof body === "string") {
      const payload = JSON.parse(body);
      for (const field of [
        "reasoning_effort",
        "thinking",
        "output_config",
        "reasoning_split",
        "max_tokens",
        "max_completion_tokens",
      ])
        delete payload[field];
      Object.assign(payload, this.input.payload);
      body = JSON.stringify(payload);
    }
    const requestSignal =
      init?.signal ?? (url instanceof Request ? url.signal : undefined);
    const signal = requestSignal
      ? AbortSignal.any([this.signal, requestSignal])
      : this.signal;
    const options: RequestInit = {
      ...init,
      body,
      headers,
      redirect: "error",
      signal,
    };
    // Keep URL/init calls intact for the SDK's fetch implementations. Request
    // inputs need a replayable template because fetch consumes their bodies.
    const template =
      url instanceof Request ? new Request(url, options) : undefined;
    for (let retry = 0; ; retry++) {
      signal.throwIfAborted();
      ++this.attempts;
      this.status = undefined;
      let response: Response | undefined;
      try {
        if (template) {
          const request = template.clone();
          response = await this.upstreamFetch(
            this.input.endpoint
              ? new Request(this.input.endpoint, request)
              : request,
          );
        } else {
          response = await this.upstreamFetch(
            this.input.endpoint || url,
            options,
          );
        }
        this.status = response.status;
      } catch (error) {
        if (
          signal.aborted &&
          !this.signal.aborted &&
          signal.reason?.name === "TimeoutError"
        )
          this.failure = new AgentFailure("timeout", "request");
        signal.throwIfAborted();
        // An SDK-local abort is terminal even if it did not abort our signal.
        if (error instanceof Error && error.name === "AbortError") throw error;
        const cause = error instanceof Error ? error.cause : undefined;
        const timeout =
          (error instanceof Error && error.name === "TimeoutError") ||
          (cause &&
            typeof cause === "object" &&
            "code" in cause &&
            [
              "ETIMEDOUT",
              "UND_ERR_CONNECT_TIMEOUT",
              "UND_ERR_HEADERS_TIMEOUT",
            ].includes(String(cause.code)));
        this.failure = new AgentFailure(
          timeout ? "timeout" : "network",
          "request",
        );
      }
      if (response?.ok) {
        this.failure = undefined;
        this.phase = "stream";
        // The body belongs to pi from this point. Never replay a response stream.
        return response;
      }
      if (response) {
        const status = response.status;
        this.failure = new AgentFailure(
          status >= 500 ? "http_5xx" : `http_${status}`,
          "request",
          status,
        );
      }
      const retryable = !response || retryStatuses.has(response.status);
      const delayMs = retryDelay(
        retry + 1,
        response?.headers.get("retry-after") ?? null,
      );
      // Discard untrusted error bodies without reading or forwarding them.
      void response?.body?.cancel().catch(() => {});
      if (!retryable || retry === maxRetries || this.hasCurrentMessageOutput())
        throw this.failure;
      signal.throwIfAborted();
      this.emit({
        type: "retry",
        attempt: retry + 1,
        max: maxRetries,
        status: response?.status,
        delayMs,
        attempts: this.attempts,
      });
      await waitForRetry(delayMs, signal);
    }
  };
}
