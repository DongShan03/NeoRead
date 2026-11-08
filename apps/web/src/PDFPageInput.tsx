import { useLayoutEffect, useState, type RefObject } from "react";
import { Input } from "@neoread/ui/components/input";

export function PDFPageInput({
  draft,
  changed,
  onNavigate,
}: {
  draft: RefObject<string>;
  changed: RefObject<((page: number) => void) | undefined>;
  onNavigate: (page: number) => void;
}) {
  const [value, setValue] = useState(() => draft.current);
  useLayoutEffect(() => {
    changed.current = (page) => setValue(String(page));
    return () => {
      changed.current = undefined;
    };
  }, [changed]);
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        const page = Number(value);
        if (Number.isInteger(page) && page > 0) onNavigate(page);
      }}
    >
      <Input
        aria-label="跳转 PDF 页码"
        value={value}
        onChange={(e) => {
          draft.current = e.target.value;
          setValue(e.target.value);
        }}
        className="h-7 w-14 text-center"
      />
    </form>
  );
}
