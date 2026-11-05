import type { Document, OverviewJob, Processing } from "@neoread/core";

/** Covers show one stage at a time, with active parsing ahead of an overview. */
export function coverStatus(
  type: Document["type"],
  job?: Processing,
  overview?: OverviewJob,
):
  | { kind: "overview"; job: OverviewJob }
  | { kind: "processing"; job: Processing }
  | { kind: "analyze" | "none" } {
  const blocked =
    job &&
    (job.incomplete || job.status === "failed" || job.status === "waiting");
  const busy =
    job &&
    (job.status === "running" ||
      job.status === "queued" ||
      (job.bookLayout && job.bookLayout.status !== "complete"));
  if (
    overview &&
    (!["complete", "canceled"].includes(overview.status) || !blocked) &&
    !busy
  )
    return { kind: "overview", job: overview };
  if (type !== "pdf") return { kind: "none" };
  return job ? { kind: "processing", job } : { kind: "analyze" };
}
