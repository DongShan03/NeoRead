import { useEffect, useState } from "react";

/** Keep unchanged job objects and arrays stable across polling snapshots. */
export function shareJobs<T extends { documentId: string }>(
  current: T[],
  next: T[],
): T[] {
  const previous = new Map(current.map((job) => [job.documentId, job]));
  const shared = next.map((job) => {
    const old = previous.get(job.documentId);
    return old && JSON.stringify(old) === JSON.stringify(job) ? old : job;
  });
  return shared.length === current.length &&
    shared.every((job, i) => job === current[i])
    ? current
    : shared;
}

/** Poll only visible windows, without overlapping requests or losing wakeups. */
export function useJobPolling<T extends { documentId: string }>(
  load: () => Promise<T[]>,
  interval: number,
) {
  const [jobs, setJobs] = useState<T[]>([]);
  const [error, setError] = useState("");
  useEffect(() => {
    let stopped = false,
      pending = false,
      refreshOnReady = false;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      if (stopped || document.hidden) return;
      if (pending) {
        refreshOnReady = true;
        return;
      }
      pending = true;
      refreshOnReady = false;
      try {
        const next = await load();
        if (!stopped && !document.hidden) {
          setJobs((current) => shareJobs(current, next));
          setError("");
        }
      } catch (e) {
        if (!stopped && !document.hidden) setError((e as Error).message);
      } finally {
        pending = false;
        if (!stopped && !document.hidden) {
          if (refreshOnReady) {
            refreshOnReady = false;
            void poll();
          } else timer = setTimeout(poll, interval);
        }
      }
    };
    const visibility = () => {
      clearTimeout(timer);
      if (!document.hidden) void poll();
    };
    document.addEventListener("visibilitychange", visibility);
    void poll();
    return () => {
      stopped = true;
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", visibility);
    };
  }, [load, interval]);
  return { jobs, error };
}
