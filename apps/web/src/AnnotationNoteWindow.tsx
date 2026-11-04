import {
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
  type PointerEvent,
} from "react";
import type { SelectionAnchor } from "@neoread/core";
import { Button } from "@neoread/ui/components/button";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from "@neoread/ui/components/dialog";
import { constrainPreviewFrame } from "./LinkPreviewPanel";

type Frame = { x: number; y: number; width: number; height: number };

export function AnnotationNoteWindow({
  open,
  onOpenChange,
  anchor,
  title,
  description,
  children,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  anchor?: SelectionAnchor;
  title: string;
  description: string;
  children: ReactNode;
}) {
  const [popup, setPopup] = useState<HTMLDivElement | null>(null);
  const [frame, setFrame] = useState<Frame | null>(null);
  const drag = useRef<{
    pointer: number;
    x: number;
    y: number;
    frame: Frame;
  } | null>(null);
  const constrain = (value: Frame) =>
    constrainPreviewFrame(value, window.innerWidth, window.innerHeight);
  useLayoutEffect(() => {
    drag.current = null;
    if (!open || !popup) {
      setFrame(null);
      return;
    }
    const width = popup.offsetWidth || 320,
      height = popup.offsetHeight || 200;
    setFrame(
      constrain({
        x: anchor
          ? (anchor.bounds?.right ?? anchor.x) + 8
          : (window.innerWidth - width) / 2,
        y: anchor
          ? (anchor.bounds?.bottom ?? anchor.bottom) + 8
          : (window.innerHeight - height) / 2,
        width,
        height,
      }),
    );
    const resize = () =>
      setFrame(
        (value) =>
          value &&
          constrain({
            ...value,
            width: popup.offsetWidth || width,
            height: popup.offsetHeight || height,
          }),
      );
    const observer = new ResizeObserver(resize);
    observer.observe(popup);
    window.addEventListener("resize", resize);
    return () => {
      observer.disconnect();
      window.removeEventListener("resize", resize);
    };
  }, [open, anchor, popup]);
  const start = (event: PointerEvent<HTMLElement>) => {
    if (event.button !== 0 || !frame) return;
    event.preventDefault();
    event.stopPropagation();
    event.currentTarget.setPointerCapture(event.pointerId);
    drag.current = {
      pointer: event.pointerId,
      x: event.clientX,
      y: event.clientY,
      frame,
    };
  };
  const move = (event: PointerEvent<HTMLElement>) => {
    const current = drag.current;
    if (!current || current.pointer !== event.pointerId) return;
    setFrame(
      constrain({
        ...current.frame,
        x: current.frame.x + event.clientX - current.x,
        y: current.frame.y + event.clientY - current.y,
      }),
    );
  };
  const stop = () => {
    drag.current = null;
  };
  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      modal={false}
      disablePointerDismissal
    >
      <DialogContent
        ref={setPopup}
        showOverlay={false}
        initialFocus={() => popup?.querySelector("textarea") ?? false}
        finalFocus={false}
        className="annotation-note-window w-80 gap-3 max-h-[calc(100dvh-24px)] overflow-y-auto"
        style={
          frame ? { left: frame.x, top: frame.y, translate: "none" } : undefined
        }
      >
        <DialogHeader>
          <DialogTitle className="pr-8">
            <Button
              variant="ghost"
              className="-m-1 h-auto w-full cursor-grab touch-none justify-start p-1 font-medium active:cursor-grabbing"
              aria-label="移动批注窗口"
              title="拖动移动，方向键微调"
              onPointerDown={start}
              onPointerMove={move}
              onPointerUp={stop}
              onPointerCancel={stop}
              onLostPointerCapture={stop}
              onKeyDown={(event) => {
                const direction = {
                  ArrowLeft: [-1, 0],
                  ArrowRight: [1, 0],
                  ArrowUp: [0, -1],
                  ArrowDown: [0, 1],
                }[event.key];
                if (!direction || !frame) return;
                event.preventDefault();
                event.stopPropagation();
                const step = event.shiftKey ? 40 : 10;
                setFrame(
                  constrain({
                    ...frame,
                    x: frame.x + direction[0] * step,
                    y: frame.y + direction[1] * step,
                  }),
                );
              }}
            >
              {title}
            </Button>
          </DialogTitle>
          <DialogDescription className="sr-only">
            {description}
          </DialogDescription>
        </DialogHeader>
        {children}
      </DialogContent>
    </Dialog>
  );
}
