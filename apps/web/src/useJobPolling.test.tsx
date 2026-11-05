// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { shareJobs, useJobPolling } from "./useJobPolling";
let root: Root, host: HTMLDivElement;
let hidden = false;
beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
  hidden = false;
  host = document.createElement("div");
  root = createRoot(host);
});
afterEach(async () => {
  await act(async () => root.unmount());
  vi.restoreAllMocks();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});
it("shares unchanged jobs and the whole snapshot", () => {
  const current = [
    { documentId: "a", count: 1 },
    { documentId: "b", count: 2 },
  ];
  expect(shareJobs(current, structuredClone(current))).toBe(current);
  const next = shareJobs(current, [current[0], { documentId: "b", count: 3 }]);
  expect(next[0]).toBe(current[0]);
  expect(next[1]).not.toBe(current[1]);
});
it.each([1200, 2000])(
  "pauses %dms polling while hidden and immediately refreshes when visible",
  async (interval) => {
    const load = vi.fn(async () => [{ documentId: "a" }]);
    let snapshot: ReturnType<typeof useJobPolling>;
    function Harness() {
      snapshot = useJobPolling(load, interval);
      return null;
    }
    await act(async () => root.render(<Harness />));
    const first = snapshot!.jobs;
    await act(async () => vi.advanceTimersByTimeAsync(interval));
    expect(load).toHaveBeenCalledTimes(2);
    expect(snapshot!.jobs).toBe(first);
    hidden = true;
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
      await vi.advanceTimersByTimeAsync(interval * 3);
    });
    expect(load).toHaveBeenCalledTimes(2);
    hidden = false;
    await act(async () =>
      document.dispatchEvent(new Event("visibilitychange")),
    );
    expect(load).toHaveBeenCalledTimes(3);
    await act(async () => root.unmount());
    await act(async () => vi.advanceTimersByTimeAsync(interval * 3));
    expect(load).toHaveBeenCalledTimes(3);
  },
);
it("starts without a request when hidden and refreshes after an in-flight visibility change", async () => {
  hidden = true;
  let resolve!: (jobs: { documentId: string }[]) => void;
  const load = vi.fn(
    () =>
      new Promise<{ documentId: string }[]>((done) => {
        resolve = done;
      }),
  );
  function Harness() {
    useJobPolling(load, 1200);
    return null;
  }
  await act(async () => root.render(<Harness />));
  expect(load).not.toHaveBeenCalled();
  hidden = false;
  await act(async () => document.dispatchEvent(new Event("visibilitychange")));
  expect(load).toHaveBeenCalledOnce();
  hidden = true;
  document.dispatchEvent(new Event("visibilitychange"));
  hidden = false;
  document.dispatchEvent(new Event("visibilitychange"));
  expect(load).toHaveBeenCalledOnce();
  await act(async () => resolve([]));
  expect(load).toHaveBeenCalledTimes(2);
});

it("does not replay a stale visibility refresh after hiding again", async () => {
  let resolve!: (jobs: { documentId: string }[]) => void;
  const load = vi.fn(async () => [] as { documentId: string }[])
    .mockImplementationOnce(() => new Promise((done) => { resolve = done; }));
  function Harness() { useJobPolling(load, 1200); return null; }
  await act(async () => root.render(<Harness />));
  hidden = true; document.dispatchEvent(new Event("visibilitychange"));
  hidden = false; document.dispatchEvent(new Event("visibilitychange"));
  hidden = true; document.dispatchEvent(new Event("visibilitychange"));
  await act(async () => resolve([]));
  hidden = false;
  await act(async () => document.dispatchEvent(new Event("visibilitychange")));
  expect(load).toHaveBeenCalledTimes(2);
  await act(async () => vi.advanceTimersByTimeAsync(1199));
  expect(load).toHaveBeenCalledTimes(2);
});
