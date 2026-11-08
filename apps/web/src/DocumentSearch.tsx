import { useEffect, useRef, useState, type RefObject } from "react";
import {
  locationLabel,
  type ReaderAdapter,
  type SearchResult,
} from "@neoread/core";
import { Button } from "@neoread/ui/components/button";
import { Input } from "@neoread/ui/components/input";
import { ScrollArea } from "@neoread/ui/components/scroll-area";
import { Search } from "lucide-react";
import { toast } from "sonner";

export type DocumentSearchState = { results: SearchResult[]; query: string };
export type SearchChanged = RefObject<
  ((state: DocumentSearchState) => void) | undefined
>;

export function DocumentSearch({
  adapter,
  inputRef,
  changed,
  onNavigate,
}: {
  adapter: ReaderAdapter | undefined;
  inputRef: RefObject<HTMLInputElement | null>;
  changed: SearchChanged;
  onNavigate: (result: SearchResult) => void;
}) {
  const [query, setQuery] = useState("");
  const [results, setResults] = useState<SearchResult[]>([]);
  const [searching, setSearching] = useState(false);
  const serial = useRef(0);
  const published = useRef(false);
  useEffect(
    () => () => {
      serial.current++;
    },
    [],
  );
  const search = async () => {
    if (!adapter) return;
    const current = ++serial.current;
    const text = query.trim();
    setSearching(true);
    try {
      const found = await adapter.search(text);
      if (current === serial.current) {
        setResults(found);
        published.current = true;
        changed.current?.({ results: found, query: text });
      }
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      if (current === serial.current) setSearching(false);
    }
  };
  return (
    <>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          void search();
        }}
        className="flex gap-1"
      >
        <Input
          aria-label="搜索文档"
          ref={inputRef}
          placeholder="搜索文档内容…"
          value={query}
          onChange={(e) => {
            serial.current++;
            setQuery(e.target.value);
            setResults([]);
            setSearching(false);
            if (published.current) {
              published.current = false;
              changed.current?.({ results: [], query: "" });
            }
          }}
        />
        <Button
          size="icon"
          variant="secondary"
          disabled={searching}
          aria-label="搜索"
        >
          <Search />
        </Button>
      </form>
      <p className="my-3 text-xs text-muted-foreground">
        {searching ? "正在搜索…" : `${results.length} 个结果`}
      </p>
      <ScrollArea className="h-[calc(100%-90px)]">
        {results.map((result) => (
          <Button
            key={result.id}
            variant="ghost"
            className="search-result"
            onClick={() => onNavigate(result)}
          >
            <small>
              {locationLabel(result.location)}
              {result.side === "translation"
                ? " · 译文"
                : result.side === "source"
                  ? " · 原文"
                  : ""}
            </small>
            <span>{result.excerpt}</span>
          </Button>
        ))}
      </ScrollArea>
    </>
  );
}
