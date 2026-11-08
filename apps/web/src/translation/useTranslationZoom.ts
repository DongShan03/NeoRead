import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type RefObject,
} from "react";
import { zoomCommand } from "@neoread/core";
import { PDFResizeObserver } from "../readers/pdf-resize";

/** Scale from the pane's fitted width, keeping the pointer's content fixed. */
export function useTranslationZoom(
  pane: RefObject<HTMLDivElement | null>,
  enabled: boolean,
  keyboardActive: () => boolean,
) {
  const [zoom, setZoom] = useState(1);
  const [width, setWidth] = useState<number>();
  const current = useRef(1);
  const keyboardTarget = useRef(keyboardActive);
  keyboardTarget.current = keyboardActive;
  const anchor = useRef<
    | { left: number; top: number; x: number; y: number; ratio: number }
    | undefined
  >(undefined);
  const change = (next: number, pointer?: { x: number; y: number }) => {
    next = Math.max(1, Math.min(5, next));
    const element = pane.current;
    if (element && next !== current.current) {
      anchor.current = {
        left: element.scrollLeft,
        top: element.scrollTop,
        x: pointer?.x ?? element.clientWidth / 2,
        y: pointer?.y ?? element.clientHeight / 2,
        ratio: next / current.current,
      };
    }
    current.current = next;
    setZoom(next);
  };
  useLayoutEffect(() => {
    const element = pane.current;
    const point = anchor.current;
    if (element && point) {
      element.scrollLeft = (point.left + point.x) * point.ratio - point.x;
      element.scrollTop = (point.top + point.y) * point.ratio - point.y;
      anchor.current = undefined;
    }
  }, [zoom, pane]);
  useEffect(() => {
    const element = pane.current;
    if (!enabled || !element) return;
    const resize = () => setWidth(Math.max(1, element.clientWidth - 32));
    resize();
    const observer = new PDFResizeObserver(element, resize, () => {});
    const canZoom = (target: EventTarget | null) =>
      !element.closest("[inert]") &&
      !document.querySelector(
        'dialog[open], [role="dialog"][aria-modal="true"], [role="alertdialog"][aria-modal="true"]',
      ) &&
      !(
        target instanceof Element &&
        target.closest(
          'input, textarea, select, [contenteditable]:not([contenteditable="false"]), [role="textbox"], dialog, [role="dialog"], [role="alertdialog"]',
        )
      );
    const wheel = (event: WheelEvent) => {
      if (
        (!event.ctrlKey && !event.metaKey) ||
        event.defaultPrevented ||
        !canZoom(event.target)
      )
        return;
      event.preventDefault();
      const bounds = element.getBoundingClientRect();
      change(current.current * Math.exp(-event.deltaY / 300), {
        x: event.clientX - bounds.left,
        y: event.clientY - bounds.top,
      });
    };
    const key = (event: KeyboardEvent) => {
      if (event.defaultPrevented || !canZoom(event.target)) return;
      const command = zoomCommand(event);
      if (!command || !keyboardTarget.current()) return;
      event.preventDefault();
      change(
        command === "width"
          ? 1
          : current.current * (command === "in" ? 1.1 : 1 / 1.1),
      );
    };
    element.addEventListener("wheel", wheel, { passive: false });
    // Claim translation shortcuts before the PDF's document bubble listener,
    // even when focus is in the toolbar or body.
    document.addEventListener("keydown", key, true);
    return () => {
      observer.disconnect();
      element.removeEventListener("wheel", wheel);
      document.removeEventListener("keydown", key, true);
    };
  }, [enabled, pane]);
  return { zoom, width, change };
}
