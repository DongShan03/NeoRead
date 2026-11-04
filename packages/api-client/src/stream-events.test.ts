import { describe, expect, it } from "vitest";
import { consumeAnnotationStream } from "./annotation-stream";
import { consumeChatStream, type ChatStreamEvent } from "./chat-stream";
import fixtures from "../../../docs/fixtures/agent-streaming-v1.json";
import { consumeCiteEvents, type CiteEvent } from "./cite-events";
import {
  consumeTranslationStream,
  type TranslationEvent,
} from "./translation-stream";

describe("annotation-stream", () => {
  it("delivers fragmented revision frames live, ignores heartbeats and cancels its reader", async () => {
    let source!: ReadableStreamDefaultController<Uint8Array>;
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        source = c;
      },
    });
    const abort = new AbortController();
    const revisions: string[] = [];
    const pending = consumeAnnotationStream(body, abort.signal, (revision) =>
      revisions.push(revision),
    );
    const revision = "a".repeat(32);
    const bytes = new TextEncoder().encode(
      `: keepalive\n\nevent: revision\ndata: "${revision}"\n\n`,
    );
    for (const byte of bytes) source.enqueue(new Uint8Array([byte]));
    for (let i = 0; i < bytes.length + 5; i++) await Promise.resolve();
    expect(revisions).toEqual([revision]);
    abort.abort();
    await expect(pending).rejects.toMatchObject({ name: "AbortError" });
    expect(body.locked).toBe(false);
  });

  it("rejects invalid revision tokens without dispatching them", async () => {
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        c.enqueue(
          new TextEncoder().encode(
            'event: revision\ndata: {"unexpected":true}\n\n',
          ),
        );
      },
    });
    const revisions: string[] = [];
    await expect(
      consumeAnnotationStream(body, new AbortController().signal, (r) =>
        revisions.push(r),
      ),
    ).rejects.toThrow("批注版本无效");
    expect(revisions).toEqual([]);
    expect(body.locked).toBe(false);
  });
});

describe("chat-stream", () => {
  const encode = (text: string) => new TextEncoder().encode(text);

  function stream(chunks: Uint8Array[]) {
    return new ReadableStream<Uint8Array>({
      start(controller) {
        chunks.forEach((chunk) => controller.enqueue(chunk));
        controller.close();
      },
    });
  }

  const fixture =
    ': comment\r\nevent:status\r\ndata: {"status":"reading"}\r\n\r\nevent: delta\r\ndata: {"text":\r\ndata: "中文 👩🏽‍💻 é\\n"}\r\n\r\nevent:done\r\ndata:{"ok":true}\r\n\r\n';

  describe("frozen SSE consumption", () => {
    it("keeps document tool activity separate from final text", async () => {
      const fixture = fixtures.find(
        (value) => value.name === "document-activity",
      )!;
      const events: ChatStreamEvent[] = [];
      await consumeChatStream(
        stream([encode(fixture.wire)]),
        new AbortController().signal,
        (event) => events.push(event),
      );
      expect(events.map((event) => event.event)).toEqual([
        "status",
        "activity",
        "activity",
        "activity",
        "delta",
        "done",
      ]);
      expect(
        events
          .filter((event) => event.event === "delta")
          .map((event) => event.data.text)
          .join(""),
      ).toBe(fixture.text);
    });
    it("preserves UTF8 across every byte boundary, CRLF and multiline data", async () => {
      const bytes = encode(fixture);
      const seen: ChatStreamEvent[] = [];
      await consumeChatStream(
        stream(Array.from(bytes, (byte) => Uint8Array.of(byte))),
        new AbortController().signal,
        (event) => seen.push(event),
      );
      expect(seen).toEqual([
        { event: "status", data: { status: "reading" } },
        { event: "delta", data: { text: "中文 👩🏽‍💻 é\n" } },
        { event: "done", data: { ok: true } },
      ]);
    });
    it("ignores unknown events and anything after terminal, even in the same chunk", async () => {
      const events: ChatStreamEvent[] = [];
      await consumeChatStream(
        stream([
          encode(
            'event: mystery\ndata: bad\n\nevent: done\ndata:{"ok":true}\n\nevent:delta\ndata:{"text":"late"}\n\nevent:done\ndata:{"ok":true}\n\n',
          ),
        ]),
        new AbortController().signal,
        (event) => events.push(event),
      );
      expect(events).toEqual([{ event: "done", data: { ok: true } }]);
    });
    it.each([
      'event:delta\ndata: {"text":7}\n\n',
      "event:done\ndata: invalid\n\n",
      'event:done\ndata:{"ok":false}\n\n',
      'event:delta\ndata:{"text":"partial"}\n\n',
      'event:done\ndata:{"ok":true}',
    ])("rejects invalid or unterminated completion %s", async (text) => {
      await expect(
        consumeChatStream(
          stream([encode(text)]),
          new AbortController().signal,
          () => {},
        ),
      ).rejects.toThrow();
    });
    it("retains delivered partial before provider error and never delivers trailing done", async () => {
      const events: ChatStreamEvent[] = [];
      await expect(
        consumeChatStream(
          stream([
            encode(
              'event:delta\ndata:{"text":"partial"}\n\nevent:error\ndata:{"error":"failed"}\n\nevent:done\ndata:{"ok":true}\n\n',
            ),
          ]),
          new AbortController().signal,
          (event) => events.push(event),
        ),
      ).rejects.toThrow("failed");
      expect(events.map((event) => event.event)).toEqual(["delta", "error"]);
    });
    it("abort releases a pending read", async () => {
      let cancelled = false;
      const controller = new AbortController();
      const pending = consumeChatStream(
        new ReadableStream({
          cancel() {
            cancelled = true;
          },
        }),
        controller.signal,
        () => {},
      );
      controller.abort();
      await expect(pending).rejects.toMatchObject({ name: "AbortError" });
      expect(cancelled).toBe(true);
    });
  });
});

describe("cite-events", () => {
  function stream(chunks: string[]) {
    const encoder = new TextEncoder();
    return new ReadableStream<Uint8Array>({
      start(controller) {
        for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
        controller.close();
      },
    });
  }

  it("dispatches commands split across chunks and skips unknown events", async () => {
    const events: CiteEvent[] = [];
    await consumeCiteEvents(
      stream([
        ": ready\n\n",
        'event: command\ndata: {"type":"command","transaction":"t","comm',
        'and":"refresh","documentId":"d","client":"wps"}\n\n',
        'event: other\ndata: {"type":"other"}\n\n',
        'event: focus\ndata: {"type":"focus"}\n\n',
      ]),
      new AbortController().signal,
      (event) => events.push(event),
    );
    expect(events).toEqual([
      {
        type: "command",
        transaction: "t",
        command: "refresh",
        documentId: "d",
        client: "wps",
      },
      { type: "focus" },
    ]);
  });
});

describe("translation-stream", () => {
  it("accepts unrequested book paragraphs alongside completed translations", async () => {
    const idle = {
      blockId: "p2-b1",
      sourceHash: "hash",
      status: "idle",
      sentences: [],
    };
    const complete = {
      ...idle,
      blockId: "p1-b1",
      status: "complete",
      sentences: [{ source: "Source.", target: "译文。" }],
    };
    const abort = new AbortController();
    const events: TranslationEvent[] = [];
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        c.enqueue(
          new TextEncoder().encode(
            `event: snapshot\ndata: ${JSON.stringify([complete, idle])}\n\nevent: translation\ndata: ${JSON.stringify(idle)}\n\n`,
          ),
        );
      },
    });
    await expect(
      consumeTranslationStream(body, abort.signal, (event) => {
        events.push(event);
        if (events.length === 2) abort.abort();
      }),
    ).rejects.toMatchObject({ name: "AbortError" });
    expect(events).toEqual([
      { event: "snapshot", data: [complete, idle] },
      { event: "translation", data: idle },
    ]);
  });

  it("delivers completed paragraphs before EOF across fragmented UTF-8 and multiple frames", async () => {
    let source!: ReadableStreamDefaultController<Uint8Array>;
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        source = c;
      },
    });
    const abort = new AbortController(),
      events: TranslationEvent[] = [];
    const running = consumeTranslationStream(body, abort.signal, (e) =>
      events.push(e),
    );
    const block = {
      blockId: "p1-b2",
      sourceHash: "hash",
      status: "complete",
      sentences: [{ source: "Source.", target: "译文。" }],
    };
    const bytes = new TextEncoder().encode(
      `event: snapshot\ndata: []\n\n: keepalive\n\nevent: translation\ndata: ${JSON.stringify(block)}\n\n`,
    );
    for (const byte of bytes) source.enqueue(new Uint8Array([byte]));
    // Let the reader consume the queued fragments without closing the connection.
    for (let i = 0; i < bytes.length + 5; i++) await Promise.resolve();
    expect(events).toEqual([
      { event: "snapshot", data: [] },
      { event: "translation", data: block },
    ]);
    source.enqueue(
      new TextEncoder().encode(
        `event: translation\ndata: ${JSON.stringify({ ...block, blockId: "p1-b1" })}\n\nevent: translation\ndata: {"blockId":`,
      ),
    );
    for (let i = 0; i < 5; i++) await Promise.resolve();
    expect(events).toHaveLength(3);
    expect(events[2]).toMatchObject({ data: { blockId: "p1-b1" } });
    abort.abort();
    await expect(running).rejects.toMatchObject({ name: "AbortError" });
  });

  it("rejects malformed events and releases the stream", async () => {
    let canceled = false;
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        c.enqueue(
          new TextEncoder().encode(
            'event: translation\ndata: {"blockId":"wrong"}\n\n',
          ),
        );
      },
      cancel() {
        canceled = true;
      },
    });
    await expect(
      consumeTranslationStream(body, new AbortController().signal, () => {}),
    ).rejects.toThrow("格式无效");
    expect(canceled).toBe(true);
  });
});
