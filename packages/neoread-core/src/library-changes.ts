const listeners = new Set<() => void>();

/** Renderer drafts and successful library writes invalidate the sync indicator. */
export function notifyLibraryChange() {
  listeners.forEach((listener) => listener());
}
export function subscribeLibraryChanges(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}
