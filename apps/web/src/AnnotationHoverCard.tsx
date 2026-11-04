import { useEffect, useRef, useState } from "react";
import type { Annotation, ReaderAnnotationTarget } from "@neoread/core";
import { Trash2 } from "lucide-react";
import { Button } from "@neoread/ui/components/button";
import {
  Popover,
  PopoverContent,
  PopoverTitle,
} from "@neoread/ui/components/popover";

export function AnnotationHoverCard({
  target,
  annotations,
  onEdit,
  onDelete,
  deleting,
}: {
  target: ReaderAnnotationTarget | null;
  annotations: Annotation[];
  onEdit: (annotation: Annotation) => void;
  onDelete: (id: string) => void;
  deleting?: ReadonlySet<string>;
}) {
  const [shown, setShown] = useState<ReaderAnnotationTarget | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const inside = useRef(false);
  const cancel = () => clearTimeout(timer.current);
  const close = () => {
    cancel();
    inside.current = false;
    setShown(null);
  };
  const leave = () => {
    cancel();
    timer.current = setTimeout(() => setShown(null), 250);
  };
  useEffect(() => {
    cancel();
    if (target) {
      timer.current = setTimeout(() => setShown(target), 150);
    } else if (!inside.current) leave();
    return cancel;
  }, [target]);
  useEffect(() => {
    const interrupt = (event: Event) => {
      if (
        event.target instanceof Element &&
        event.target.closest("[data-annotation-preview]")
      )
        return;
      close();
    };
    window.addEventListener("scroll", interrupt, true);
    window.addEventListener("resize", interrupt);
    return () => {
      cancel();
      window.removeEventListener("scroll", interrupt, true);
      window.removeEventListener("resize", interrupt);
    };
  }, []);
  const notes = shown
    ? annotations.filter((a) => shown.ids.includes(a.id) && a.note.trim())
    : [];
  return (
    <Popover
      open={notes.length > 0}
      onOpenChange={(open) => {
        if (!open) close();
      }}
    >
      {shown && notes.length > 0 && (
        <PopoverContent
          anchor={{
            getBoundingClientRect: () =>
              new DOMRect(
                shown.anchor.x,
                shown.anchor.top,
                1,
                Math.max(1, shown.anchor.bottom - shown.anchor.top),
              ),
          }}
          side="right"
          align="start"
          sideOffset={12}
          initialFocus={false}
          finalFocus={false}
          className="max-h-[min(24rem,70vh)] w-72 max-w-[calc(100vw-2rem)] overflow-y-auto"
          data-annotation-preview=""
          onPointerEnter={() => {
            inside.current = true;
            cancel();
          }}
          onPointerLeave={() => {
            inside.current = false;
            leave();
          }}
          onFocusCapture={() => {
            inside.current = true;
            cancel();
          }}
          onBlurCapture={(event) => {
            if (!event.currentTarget.contains(event.relatedTarget)) {
              inside.current = false;
              leave();
            }
          }}
        >
          <PopoverTitle className="sr-only">批注预览</PopoverTitle>
          {notes.map((annotation) => (
            <div
              key={annotation.id}
              className="border-b pb-2 last:border-0 last:pb-0"
            >
              <Button
                variant="ghost"
                className="h-auto w-full justify-start whitespace-pre-wrap break-words p-0 text-left text-sm font-normal leading-relaxed"
                aria-label="编辑批注内容"
                disabled={deleting?.has(annotation.id)}
                onClick={() => {
                  close();
                  onEdit(annotation);
                }}
              >
                {annotation.note}
              </Button>
              <div className="mt-1 flex justify-end">
                <Button
                  size="icon-sm"
                  variant="ghost"
                  aria-label="删除批注"
                  disabled={deleting?.has(annotation.id)}
                  onClick={() => {
                    onDelete(annotation.id);
                  }}
                >
                  <Trash2 className="size-3.5" />
                </Button>
              </div>
            </div>
          ))}
        </PopoverContent>
      )}
    </Popover>
  );
}
