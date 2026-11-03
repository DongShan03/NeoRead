import { expect, it } from "vitest";
import { mkdtemp, mkdir, writeFile, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { LibraryCatalog } from "./library-catalog";

it("serializes cloud rename notification with the active local library rename", async () => {
  const root = await mkdtemp(join(tmpdir(), "neoread-sync-catalog-"));
  try {
    const active = join(root, "library");
    await mkdir(active);
    await writeFile(join(active, "neoread.sqlite"), "");
    const catalog = new LibraryCatalog(root);
    const initial = await catalog.list(active);
    const entry = initial.libraries.find(
      (item) => item.id === initial.activeId,
    )!;
    const notified: string[] = [];
    const renamed = await catalog.rename(
      active,
      entry.id,
      "研究论文",
      async (name) => {
        notified.push(name);
      },
    );
    expect(notified).toEqual(["研究论文"]);
    expect(renamed.libraries.find((item) => item.id === entry.id)?.name).toBe(
      "研究论文",
    );
    await expect(
      catalog.rename(active, entry.id, "失败更名", async () => {
        throw new Error("cannot queue rename");
      }),
    ).rejects.toThrow("cannot queue rename");
    expect(
      (await catalog.list(active)).libraries.find(
        (item) => item.id === entry.id,
      )?.name,
    ).toBe("研究论文");
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

it("queues cloud renames for an inactive library without contacting the active server", async () => {
  const root = await mkdtemp(join(tmpdir(), "neoread-sync-catalog-"));
  try {
    const active = join(root, "library");
    const inactive = join(root, "inactive");
    await mkdir(active);
    await mkdir(inactive);
    await writeFile(join(active, "neoread.sqlite"), "");
    await writeFile(
      join(inactive, "webdav-sync.json"),
      JSON.stringify({ account: "reader@example.com" }),
    );
    const catalog = new LibraryCatalog(root);
    await catalog.list(active);
    await catalog.remember({
      id: "inactive",
      name: "书库二",
      directory: inactive,
    });
    const result = await catalog.rename(
      active,
      "inactive",
      "研究图书",
      async () => {
        throw new Error("must not use active server");
      },
    );
    expect(
      result.libraries.find((entry) => entry.id === "inactive")?.name,
    ).toBe("研究图书");
    expect(
      await readFile(join(inactive, "webdav-library-name.json"), "utf8"),
    ).toBe("研究图书");
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
