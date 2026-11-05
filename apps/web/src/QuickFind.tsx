import { useEffect, useId, useMemo, useState } from "react";
import {
  BookOpen,
  FileText,
  Command,
  Settings2,
  CirclePlus,
  Library,
} from "lucide-react";
import type { Document, LibraryMode } from "@neoread/core";
import { Button } from "@neoread/ui/components/button";
import { Input } from "@neoread/ui/components/input";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from "@neoread/ui/components/dialog";
import { filterDocuments, initialFilters } from "./library";

type Props = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  documents: Document[];
  mode: LibraryMode;
  onDocument: (document: Document) => void;
  onSettings: () => void;
  onImport: () => void;
  onSwitchLibrary: () => void;
};

export function QuickFind(props: Props) {
  return (
    <Dialog open={props.open} onOpenChange={props.onOpenChange}>
      {props.open && <QuickFindContent {...props} />}
    </Dialog>
  );
}

function QuickFindContent({
  documents,
  mode,
  onDocument,
  onSettings,
  onImport,
  onSwitchLibrary,
  onOpenChange,
}: Props) {
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState(0);
  const listId = useId();
  const results = useMemo(
    () =>
      query.trim()
        ? filterDocuments(documents, "all", query, initialFilters).slice(0, 50)
        : [],
    [documents, query],
  );
  const actions = [
    { id: "settings", title: "打开设置", icon: Settings2, run: onSettings },
    {
      id: "import",
      title: mode === "papers" ? "导入论文" : "导入文档",
      icon: CirclePlus,
      run: onImport,
    },
    {
      id: "library",
      title: mode === "papers" ? "切换到图书库" : "切换到论文库",
      icon: Library,
      run: onSwitchLibrary,
    },
  ].filter((action) => !query.trim() || action.title.includes(query.trim()));
  const items = [
    ...results.map((doc) => ({
      id: doc.id,
      title: doc.title,
      author: doc.author,
      icon: doc.type === "epub" ? BookOpen : FileText,
      run: () => onDocument(doc),
    })),
    ...actions.map((action) => ({ ...action, author: "" })),
  ];
  const index = Math.min(selected, Math.max(0, items.length - 1));
  const optionId = (index: number) => `${listId}-${index}`;
  useEffect(() => {
    document
      .getElementById(optionId(index))
      ?.scrollIntoView?.({ block: "nearest" });
  }, [index, query]);
  const execute = (index: number) => {
    const item = items[index];
    if (!item) return;
    onOpenChange(false);
    item.run();
  };
  return (
    <DialogContent data-quick-find>
      <DialogHeader>
        <DialogTitle className="flex items-center gap-2">
          <Command size={18} />
          快速查找
        </DialogTitle>
        <DialogDescription className="sr-only">
          搜索书库，或打开常用操作。
        </DialogDescription>
      </DialogHeader>
      <Input
        autoFocus
        aria-label="快速查找"
        role="combobox"
        aria-autocomplete="list"
        aria-expanded="true"
        aria-controls={listId}
        aria-activedescendant={items.length ? optionId(index) : undefined}
        placeholder="搜索标题、作者、标签…"
        value={query}
        onChange={(event) => {
          setQuery(event.target.value);
          setSelected(0);
        }}
        onKeyDown={(event) => {
          if (event.nativeEvent.isComposing) return;
          if (event.key === "ArrowDown" || event.key === "ArrowUp") {
            event.preventDefault();
            if (items.length)
              setSelected(
                (index + (event.key === "ArrowDown" ? 1 : -1) + items.length) %
                  items.length,
              );
          } else if (event.key === "Enter") {
            event.preventDefault();
            execute(index);
          } else if (event.key === "Escape") {
            event.preventDefault();
            event.stopPropagation();
            onOpenChange(false);
          }
        }}
      />
      <div
        id={listId}
        role="listbox"
        aria-label="查找结果"
        className="min-w-0 max-h-72 space-y-1 overflow-auto"
      >
        {!items.length && (
          <p
            role="status"
            className="py-6 text-center text-sm text-muted-foreground"
          >
            没有匹配的文档
          </p>
        )}
        {items.map((item, position) => (
          <Button
            key={item.id}
            id={optionId(position)}
            role="option"
            aria-selected={position === index}
            variant={position === index ? "secondary" : "ghost"}
            tabIndex={-1}
            className="h-auto min-w-0 w-full justify-start py-2"
            title={item.title}
            onPointerMove={() => setSelected(position)}
            onPointerDown={(event) => event.preventDefault()}
            onClick={() => execute(position)}
          >
            <item.icon className="shrink-0" />
            <span className="min-w-0 flex-1 text-left">
              <span className="block truncate">{item.title}</span>
              {item.author && (
                <span className="block truncate text-xs text-muted-foreground">
                  {item.author}
                </span>
              )}
            </span>
          </Button>
        ))}
      </div>
    </DialogContent>
  );
}
