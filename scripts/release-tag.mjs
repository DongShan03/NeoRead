// Public experiment labels use dots; Electron package versions use SemVer.
export const canonicalReleaseTag = (tag) =>
  tag?.replace(/^v(\d+\.\d+\.\d+)\.experiment\.(\d+)$/, "v$1-experiment.$2");
