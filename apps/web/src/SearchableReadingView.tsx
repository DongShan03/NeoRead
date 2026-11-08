import { useLayoutEffect, useState, type ComponentProps } from "react";
import { PDFReadingView } from "./translation/PDFReadingView";
import { EPUBReadingView } from "./translation/EPUBReadingView";
import type { DocumentSearchState, SearchChanged } from "./DocumentSearch";

// Search result commits and clearing highlights update only the reading view.
export function SearchableReadingView({
  searchChanged,
  ...props
}: Omit<
  ComponentProps<typeof PDFReadingView>,
  "searchResults" | "searchQuery"
> & {
  searchChanged: SearchChanged;
}) {
  const [search, setSearch] = useState<DocumentSearchState>({
    results: [],
    query: "",
  });
  useLayoutEffect(() => {
    searchChanged.current = setSearch;
    return () => {
      searchChanged.current = undefined;
    };
  }, [searchChanged]);
  const ReadingView =
    props.document.type === "pdf" ? PDFReadingView : EPUBReadingView;
  return (
    <ReadingView
      {...props}
      searchResults={search.results}
      searchQuery={search.query}
    />
  );
}
