import { expect, it } from "vitest";
import type { OverviewJob, Processing } from "@neoread/core";
import { coverStatus } from "./cover-status";
const job = (patch: Partial<Processing> = {}) =>
  ({
    documentId: "a",
    phase: "ready",
    status: "complete",
    ...patch,
  }) as Processing;
const overview = (status: OverviewJob["status"]) =>
  ({ documentId: "a", status }) as OverviewJob;
it("offers analysis only for PDFs without processing or overview", () => {
  expect(coverStatus("pdf").kind).toBe("analyze");
  expect(coverStatus("epub").kind).toBe("none");
  expect(coverStatus("epub", undefined, overview("queued")).kind).toBe(
    "overview",
  );
});
it.each(["running", "queued"] as const)(
  "prioritizes %s processing over every overview stage",
  (status) => {
    for (const state of ["running", "failed", "complete", "canceled"] as const)
      expect(coverStatus("pdf", job({ status }), overview(state)).kind).toBe(
        "processing",
      );
  },
);
it("keeps unfinished book layout ahead of overview", () => {
  expect(
    coverStatus(
      "pdf",
      job({ bookLayout: { status: "queued", pagesDone: 0, pagesTotal: 2 } }),
      overview("running"),
    ).kind,
  ).toBe("processing");
});
it.each([
  { status: "failed" },
  { status: "waiting" },
  { incomplete: true },
] as const)(
  "keeps parsing warnings after overview completion (%j)",
  (patch) => {
    for (const state of ["complete", "canceled"] as const)
      expect(coverStatus("pdf", job(patch), overview(state)).kind).toBe(
        "processing",
      );
    expect(coverStatus("pdf", job(patch), overview("running")).kind).toBe(
      "overview",
    );
  },
);
it("passes through the chosen job for completed or paused processing", () => {
  const processing = job({ status: "paused" });
  const o = overview("complete");
  expect(coverStatus("pdf", processing, o)).toEqual({
    kind: "overview",
    job: o,
  });
  expect(coverStatus("pdf", processing)).toEqual({
    kind: "processing",
    job: processing,
  });
});
