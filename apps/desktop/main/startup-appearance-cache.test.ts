import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, it } from "vitest";
import {
  readStartupAppearance,
  saveStartupAppearance,
} from "./startup-appearance-cache";

it("persists the last chosen appearance and falls back safely for missing/corrupt cache", async () => {
  const directory = await mkdtemp(join(tmpdir(), "neoread-startup-theme-"));
  try {
    expect(readStartupAppearance(directory)).toEqual({
      appearance: "system",
      reducedMotion: false,
    });
    await Promise.all([
      saveStartupAppearance(directory, "dark"),
      saveStartupAppearance(directory, "light"),
    ]);
    expect(readStartupAppearance(directory)).toEqual({
      appearance: "light",
      reducedMotion: false,
    });
    expect(
      JSON.parse(
        await readFile(join(directory, "startup-appearance.json"), "utf8"),
      ),
    ).toEqual({ appearance: "light", reducedMotion: false });
    await writeFile(join(directory, "startup-appearance.json"), "broken");
    expect(readStartupAppearance(directory)).toEqual({
      appearance: "system",
      reducedMotion: false,
    });
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

it("preserves both hints through serialized partial updates and reads legacy caches", async () => {
  const directory = await mkdtemp(join(tmpdir(), "neoread-startup-motion-"));
  const file = join(directory, "startup-appearance.json");
  try {
    await writeFile(file, JSON.stringify({ appearance: "sepia" }));
    expect(readStartupAppearance(directory)).toEqual({
      appearance: "sepia",
      reducedMotion: false,
    });
    await Promise.all([
      saveStartupAppearance(directory, undefined, true),
      saveStartupAppearance(directory, "dark"),
    ]);
    expect(readStartupAppearance(directory)).toEqual({
      appearance: "dark",
      reducedMotion: true,
    });
    await saveStartupAppearance(directory, undefined, false);
    expect(readStartupAppearance(directory)).toEqual({
      appearance: "dark",
      reducedMotion: false,
    });
    expect((await readFile(file)).length).toBeLessThanOrEqual(256);
    await writeFile(file, " ".repeat(257));
    expect(readStartupAppearance(directory)).toEqual({
      appearance: "system",
      reducedMotion: false,
    });
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
