import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import type { WebDAVConfig } from "../../../packages/neoread-core/src/webdav-sync";

// Only an allocated new directory is passed here. The verified cloud identity
// stays independent of the unique local display name (e.g. 论文库(1)).
export async function prepareCloudLibrary(
  directory: string,
  source: string,
  config: WebDAVConfig,
) {
  let password: string | undefined;
  try {
    const previous = JSON.parse(
      await readFile(join(source, "webdav-sync.json"), "utf8"),
    );
    if (
      previous.account === config.account &&
      typeof previous.password === "string"
    )
      password = previous.password;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
  await mkdir(directory);
  await writeFile(
    join(directory, "webdav-sync.json"),
    JSON.stringify({
      account: config.account,
      password,
      remoteId: config.remoteId,
      folder: config.folder,
      deviceName: config.deviceName,
      intervalMinutes: config.intervalMinutes,
      enabled: true,
    }) + "\n",
    { flag: "wx", mode: 0o600 },
  );
}
