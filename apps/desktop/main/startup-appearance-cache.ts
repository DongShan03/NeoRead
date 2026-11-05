import { closeSync, fstatSync, openSync, readFileSync } from "node:fs";
import { mkdir, rename, writeFile } from "node:fs/promises";
import { join } from "node:path";

export type StartupAppearance = "system" | "light" | "sepia" | "dark";
const valid = (value: unknown): value is StartupAppearance =>
  value === "system" ||
  value === "light" ||
  value === "sepia" ||
  value === "dark";
const filename = "startup-appearance.json";
export type StartupAppearanceCache = {
  appearance: StartupAppearance;
  reducedMotion: boolean;
};
const defaults = (): StartupAppearanceCache => ({
  appearance: "system",
  reducedMotion: false,
});

/** A tiny local hint; authoritative reader settings still come from the library. */
export function readStartupAppearance(
  directory: string,
): StartupAppearanceCache {
  let fd: number | undefined;
  try {
    fd = openSync(join(directory, filename), "r");
    const stat = fstatSync(fd);
    if (!stat.isFile() || stat.size > 256) return defaults();
    const value: unknown = JSON.parse(readFileSync(fd, "utf8"));
    const { appearance, reducedMotion } = (value ?? {}) as {
      appearance?: unknown;
      reducedMotion?: unknown;
    };
    return {
      appearance: valid(appearance) ? appearance : "system",
      reducedMotion: reducedMotion === true,
    };
  } catch {
    return defaults();
  } finally {
    if (fd !== undefined) closeSync(fd);
  }
}

let pending = Promise.resolve();
export function saveStartupAppearance(
  directory: string,
  appearance?: unknown,
  reducedMotion?: unknown,
) {
  // Serialize changes so an older write cannot replace the user's latest choice.
  pending = pending
    .catch(() => {})
    .then(async () => {
      const value = readStartupAppearance(directory);
      if (appearance !== undefined)
        value.appearance = valid(appearance) ? appearance : "system";
      if (typeof reducedMotion === "boolean")
        value.reducedMotion = reducedMotion;
      await mkdir(directory, { recursive: true });
      const target = join(directory, filename);
      const temporary = `${target}.tmp`;
      await writeFile(temporary, JSON.stringify(value), {
        mode: 0o600,
      });
      await rename(temporary, target);
    });
  return pending;
}
