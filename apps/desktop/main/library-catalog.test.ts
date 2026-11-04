import { expect, it } from "vitest";
import { mkdtemp, mkdir, writeFile, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { LibraryCatalog } from "./library-catalog";
import { prepareCloudLibrary } from "./cloud-library";
import type { WebDAVConfig } from "../../../packages/neoread-core/src/webdav-sync";

it("connects a cloud-only library with sync enabled without renaming or overwriting an existing local library", async () => {
  const root = await mkdtemp(join(tmpdir(), "neoread-cloud-catalog-"));
  try {
    const active = join(root, "library");
    await mkdir(active);
    await writeFile(join(active, "neoread.sqlite"), "original");
    await writeFile(
      join(active, "webdav-sync.json"),
      JSON.stringify({
        account: "reader@example.com",
        password: "private-password",
        enabled: false,
      }),
    );
    const catalog = new LibraryCatalog(root);
    const entry = await catalog.allocate(active, "论文库");
    const config = {
      account: "reader@example.com",
      remoteId: "a".repeat(32),
      folder: "论文库",
      deviceName: "Mac",
      enabled: true,
      passwordSet: true,
      intervalMinutes: 15,
    } satisfies WebDAVConfig;
    await prepareCloudLibrary(entry.directory, active, config);
    await writeFile(join(entry.directory, "neoread.sqlite"), "new");
    await catalog.remember(entry);
    const snapshot = await catalog.list(entry.directory, true);
    expect(
      snapshot.libraries.find((item) => item.id === entry.id),
    ).toMatchObject({
      name: "论文库(1)",
      sync: {
        enabled: true,
        remoteId: config.remoteId,
        account: config.account,
      },
    });
    expect(
      snapshot.libraries.find((item) => item.id !== entry.id)?.sync?.enabled,
    ).toBe(false);
    expect(JSON.stringify(snapshot)).not.toContain("private-password");
    const saved = JSON.parse(
      await readFile(join(entry.directory, "webdav-sync.json"), "utf8"),
    );
    expect(saved).toMatchObject({
      enabled: true,
      remoteId: config.remoteId,
      folder: "论文库",
      password: "private-password",
    });
    await expect(
      prepareCloudLibrary(entry.directory, active, config),
    ).rejects.toThrow();
    expect(await readFile(join(active, "neoread.sqlite"), "utf8")).toBe(
      "original",
    );
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

it("deletes a renamed default library only after switching, rolls back failed deletion, and keeps the last library", async () => {
  const root = await mkdtemp(join(tmpdir(), "neoread-delete-catalog-"));
  try {
    const original = join(root, "library"),
      other = join(root, "other");
    for (const directory of [original, other]) {
      await mkdir(directory);
      await writeFile(join(directory, "neoread.sqlite"), "");
    }
    const catalog = new LibraryCatalog(root);
    const initial = await catalog.list(original);
    const id = initial.activeId!;
    await catalog.rename(original, id, "重命名的默认库");
    await catalog.remember({ id: "other", name: "其他库", directory: other });
    await expect(catalog.remove(original, id, async () => {})).rejects.toThrow(
      "先切换",
    );
    await expect(
      catalog.remove(other, id, async () => {
        throw new Error("trash failed");
      }),
    ).rejects.toThrow("trash failed");
    expect((await catalog.list(other)).libraries).toHaveLength(2);
    await catalog.remove(other, id, (directory) =>
      rm(directory, { recursive: true }),
    );
    const remaining = await catalog.list(other);
    expect(remaining.libraries.map((entry) => entry.id)).toEqual(["other"]);
    await expect(
      catalog.remove(other, "other", async () => {}),
    ).rejects.toThrow("至少需要保留一个书库");
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

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
