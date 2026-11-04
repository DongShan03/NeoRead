import { describe, expect, it } from "vitest";
import { type Document, type Processing } from "@neoread/core";
import {
  filterDocuments,
  initialFilters,
  libraryTags,
  matchTagBoard,
} from "./library";
import { processingStages } from "./ProcessingStatus";
import { cardScales, nextCardScale } from "./useCardZoom";
import { migrateReaderPreferences } from "./legacy-preferences";

describe("library", () => {
  const makeDoc = (patch: Partial<Document>): Document => ({
    id: "1",
    type: "pdf",
    title: "Security",
    author: "Alice",
    size: 100,
    createdAt: "2026-10-01",
    lastOpenedAt: "2026-10-02",
    favorite: false,
    percentage: 0,
    category: "paper",
    categorySource: "ai",
    classificationStatus: "done",
    classificationError: "",
    library: "books",
    metadata: {},
    readingStatus: "unread",
    noteCount: 0,
    highlightCount: 0,
    openQuestionCount: 0,
    related: [],
    folders: [],
    tags: [],
    ...patch,
  });

  const documents = [
    makeDoc({
      id: "paper",
      favorite: true,
      tags: ["Web", "安全"],
      percentage: 0.5,
    }),
    makeDoc({
      id: "article",
      category: "article",
      tags: ["web"],
      percentage: 1,
    }),
    makeDoc({
      id: "book",
      category: "book",
      type: "epub",
      tags: ["安全"],
      title: "A book",
    }),
  ];

  it("combines format, reading, favorite, search and all selected tags", () => {
    const filters = {
      ...initialFilters,
      format: "pdf",
      reading: "reading",
      tags: ["web", "安全"],
    };
    expect(
      filterDocuments(documents, "favorites", " alice ", filters).map(
        (d) => d.id,
      ),
    ).toEqual(["paper"]);
    expect(
      filterDocuments(documents, "all", "", {
        ...initialFilters,
        tags: ["WEB", "安全"],
      }).map((d) => d.id),
    ).toEqual(["paper"]);
    expect(
      filterDocuments(documents, "all", "", { ...filters, format: "epub" }),
    ).toEqual([]);
  });

  it("supports unread/finished boundaries and title/tag search without mutating the library", () => {
    expect(
      filterDocuments(documents, "all", "", {
        ...initialFilters,
        reading: "unread",
      }).map((d) => d.id),
    ).toEqual(["book"]);
    expect(
      filterDocuments(documents, "all", "", {
        ...initialFilters,
        reading: "finished",
      }).map((d) => d.id),
    ).toEqual(["article"]);
    expect(
      filterDocuments(documents, "all", "安全", initialFilters).map(
        (d) => d.id,
      ),
    ).toEqual(["paper", "book"]);
    expect(
      filterDocuments(documents, "all", "", {
        ...initialFilters,
        sort: "title",
      })[0].id,
    ).toBe("book");
    expect(documents[0].id).toBe("paper");
    expect(
      libraryTags(documents).filter((tag) => tag.toLowerCase() === "web"),
    ).toEqual(["Web"]);
  });

  it("matches saved tag combinations in all/any mode and updates when document tags change", () => {
    const board = { tags: ["WEB", "安全"], match: "all" as const };
    expect(matchTagBoard(documents, board).map((d) => d.id)).toEqual(["paper"]);
    expect(
      matchTagBoard(documents, { ...board, match: "any" }).map((d) => d.id),
    ).toEqual(["paper", "article", "book"]);
    const updated = documents.map((d) =>
      d.id === "article" ? { ...d, tags: ["web", "安全"] } : d,
    );
    expect(matchTagBoard(updated, board).map((d) => d.id)).toEqual([
      "paper",
      "article",
    ]);
    expect(
      matchTagBoard(documents, { tags: ["deleted-tag"], match: "all" }),
    ).toEqual([]);
    expect(board.tags).toEqual(["WEB", "安全"]);
  });
});

describe("processing", () => {
  const job: Processing = {
    documentId: "test",
    phase: "learning",
    status: "running",
    pagesDone: 3,
    pagesTotal: 13,
    translationsDone: 0,
    translationsTotal: 0,
    detail: "",
    updatedAt: "",
  };

  it("shows measured learning progress without a consolidation stage", () => {
    const stages = processingStages(job);
    expect(stages.map((stage) => stage.key)).toEqual([
      "learning",
      "translating",
    ]);
    expect(stages[0].value).toBeCloseTo((3 / 13) * 100);
    expect(stages[1].value).toBe(0);
    expect(stages[1].active).toBe(false);
  });

  it("measures translation after learning", () => {
    for (const status of ["running", "waiting", "failed"] as const) {
      const stages = processingStages({
        ...job,
        phase: "translating",
        status,
        translationsDone: 92,
        translationsTotal: 183,
      });
      expect(stages[0].value).toBe(100);
      expect(stages[1].label).toBe("翻译中");
      expect(stages[1].value).toBeCloseTo((92 / 183) * 100);
      expect(stages[1].count).toBe("92 / 183 段");
      expect(stages[1].active).toBe(status === "running");
    }
  });
});

describe("card-zoom", () => {
  it("steps card sizes within the limits and snaps saved values between steps", () => {
    expect(nextCardScale(1, 1)).toBe(1.15);
    expect(nextCardScale(1, -1)).toBe(0.9);
    expect(nextCardScale(cardScales[0], -1)).toBe(cardScales[0]);
    expect(nextCardScale(cardScales.at(-1)!, 1)).toBe(cardScales.at(-1));
    expect(nextCardScale(1.05, 1)).toBe(1.15);
    expect(nextCardScale(1.05, -1)).toBe(1);
  });
});

describe("legacy-preferences", () => {
  function memoryStorage(): Storage {
    const values = new Map<string, string>();
    return {
      get length() {
        return values.size;
      },
      key: (index) => [...values.keys()][index] ?? null,
      getItem: (key) => values.get(key) ?? null,
      setItem: (key, value) => {
        values.set(key, value);
      },
      removeItem: (key) => {
        values.delete(key);
      },
      clear: () => values.clear(),
    };
  }

  describe("Reader preference migration", () => {
    it("converts known keys, preserves newer values and unrelated content", () => {
      const storage = memoryStorage();
      storage.setItem("reader.mark-color", "yellow");
      storage.setItem("neoread.mark-color", "blue");
      storage.setItem("reader.notes-export", '["notes"]');
      storage.setItem("reader.chat-session.doc", "session");
      storage.setItem("reader.unrelated", "original");
      migrateReaderPreferences(storage);
      expect(storage.getItem("neoread.mark-color")).toBe("blue");
      expect(storage.getItem("neoread.notes-export")).toBe('["notes"]');
      expect(storage.getItem("neoread.chat-session.doc")).toBe("session");
      expect(storage.getItem("reader.unrelated")).toBe("original");
      expect(storage.getItem("reader.notes-export")).toBeNull();
      storage.setItem("reader.chat-session.other", "old");
      migrateReaderPreferences(storage);
      expect(storage.getItem("neoread.chat-session.other")).toBeNull();
    });
    it("retains old values after a write failure and can retry", () => {
      const storage = memoryStorage();
      storage.setItem("reader.mark-color", "yellow");
      const write = storage.setItem;
      storage.setItem = () => {
        throw new Error("quota");
      };
      expect(() => migrateReaderPreferences(storage)).not.toThrow();
      expect(storage.getItem("reader.mark-color")).toBe("yellow");
      storage.setItem = write;
      migrateReaderPreferences(storage);
      expect(storage.getItem("neoread.mark-color")).toBe("yellow");
    });
  });
});
