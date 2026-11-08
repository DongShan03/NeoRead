export type FailureCode =
  | `http_${number}`
  | "http_5xx"
  | "network"
  | "timeout"
  | "stream_interrupted"
  | "stop_length"
  | "stop_other"
  | "empty_answer"
  | "output_limit"
  | "invalid_message";

// Only fixed messages cross the worker boundary. SDK errors can contain bodies
// and credentials even when the transport error itself was safe.
export class AgentFailure extends Error {
  constructor(
    readonly code: FailureCode,
    readonly phase: "request" | "stream" | "validation",
    readonly status?: number,
  ) {
    super(`Agent failed (${code})`);
  }
}
