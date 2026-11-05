// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { beforeEach, afterEach, expect, it, vi } from "vitest";
import { api } from "@neoread/api";
import { TagBoards } from "./TagBoards";
import { paper } from "./papers/fixtures";
import type { LibraryDocuments } from "./LibraryDocuments";
import type { ComponentProps } from "react";
const captured = vi.hoisted(() => ({
  props: undefined as ComponentProps<typeof LibraryDocuments> | undefined,
}));
vi.mock("@neoread/api", () => ({
  api: {
    tagBoards: vi.fn(),
    updateTagBoard: vi.fn(),
    createTagBoard: vi.fn(),
    removeTagBoard: vi.fn(),
  },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("./LibraryDocuments", () => ({
  LibraryDocuments: (props: ComponentProps<typeof LibraryDocuments>) => {
    captured.props = props;
    return (
      <div data-cards>
        {props.documents.map((doc) => (
          <article key={doc.id}>{doc.title}</article>
        ))}
      </div>
    );
  },
}));
const board = {
  id: "board",
  name: "Web Security",
  tags: ["web"],
  match: "all" as const,
};
let root: Root, host: HTMLDivElement;
beforeEach(() => {
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  vi.clearAllMocks();
  vi.mocked(api.tagBoards).mockResolvedValue([board]);
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
  vi.unstubAllGlobals();
});
async function render() {
  await act(async () =>
    root.render(
      <TagBoards
        documents={[]}
        query=""
        jobs={[]}
        processingError=""
        openDocument={() => {}}
        favorite={() => {}}

        onSettings={() => {}}
        onOverview={() => {}}
        onUsage={() => {}}
      />,
    ),
  );
}
const button = (text: string) =>
  [...document.querySelectorAll<HTMLButtonElement>("button")].find(
    (b) => b.textContent === text,
  )!;
it("keeps a board on deletion failure and removes it only after acknowledgement", async () => {
  await render();
  await act(async () =>
    (
      document.querySelector(
        '[aria-label="删除看板 Web Security"]',
      ) as HTMLButtonElement
    ).click(),
  );
  vi.mocked(api.removeTagBoard).mockRejectedValueOnce(Error("offline"));
  await act(async () => button("删除看板").click());
  expect(document.querySelector('[role="dialog"]')).not.toBeNull();
  expect(host.querySelector(".tag-board-card")).not.toBeNull();
  vi.mocked(api.removeTagBoard).mockResolvedValue(undefined);
  await act(async () => button("删除看板").click());
  expect(host.querySelector(".tag-board-card")).toBeNull();
});
it("retains edited board values on save failure for retry", async () => {
  await render();
  await act(async () =>
    (
      document.querySelector(
        '[aria-label="编辑看板 Web Security"]',
      ) as HTMLButtonElement
    ).click(),
  );
  const name = document.querySelector<HTMLInputElement>("#board-name")!;
  await act(async () => {
    Object.getOwnPropertyDescriptor(
      HTMLInputElement.prototype,
      "value",
    )!.set!.call(name, "Renamed");
    name.dispatchEvent(new Event("input", { bubbles: true }));
  });
  vi.mocked(api.updateTagBoard).mockRejectedValueOnce(Error("offline"));
  await act(async () => button("保存").click());
  expect(name.value).toBe("Renamed");
  expect(document.querySelector('[role="dialog"]')).not.toBeNull();
  vi.mocked(api.updateTagBoard).mockResolvedValue({
    ...board,
    name: "Renamed",
  });
  await act(async () => button("保存").click());
  expect(api.updateTagBoard).toHaveBeenLastCalledWith("board", {
    name: "Renamed",
    tags: ["web"],
    match: "all",
  });
  expect(host.textContent).toContain("Renamed");
});
it("offers retry after failing to load saved boards", async () => {
  vi.mocked(api.tagBoards).mockRejectedValueOnce(Error("offline"));
  await render();
  expect(host.querySelector('[role="alert"]')?.textContent).toContain(
    "offline",
  );
  await act(async () => button("重新加载看板").click());
  expect(host.querySelector(".tag-board-card")?.textContent).toContain(
    "Web Security",
  );
});

it("reloads saved boards after library data arrives from sync", async () => {
  await render();
  vi.mocked(api.tagBoards).mockResolvedValue([
    { ...board, name: "Synced board" },
  ]);
  await act(async () => {
    window.dispatchEvent(
      new CustomEvent("neoread:sync-applied", { detail: ["library"] }),
    );
  });
  expect(host.textContent).toContain("Synced board");
});

it("keeps matching board card nodes during search and forwards menu and batch actions", async () => {
  const documents = [
    paper({ id: "a", library: "books", title: "Alpha", tags: ["web"] }),
  ];
  const onMove = vi.fn(),
    openDocuments = vi.fn();
  const show = (query: string) =>
    root.render(
      <TagBoards
        documents={documents}
        query={query}
        jobs={[]}
        processingError=""
        openDocument={() => {}}
        favorite={() => {}}
        onSettings={() => {}}
        onOverview={() => {}}
        onUsage={() => {}}
        onMove={onMove}
        openDocuments={openDocuments}
      />,
    );
  await act(async () => show(""));
  await act(async () => button("Web Security").click());
  const card = host.querySelector("[data-cards] article");
  expect(card).not.toBeNull();
  await act(async () => show("Al"));
  expect(host.querySelector("[data-cards] article")).toBe(card);
  expect(captured.props!.onMove).toBe(onMove);
  expect(captured.props!.openDocuments).toBe(openDocuments);
});
