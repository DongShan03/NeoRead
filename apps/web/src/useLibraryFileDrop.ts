import { useEffect, useRef, useState, type DragEvent } from "react";
const hasFiles = (event: DragEvent) => event.dataTransfer.types.includes("Files");

export function useLibraryFileDrop(onFiles: (files: File[]) => void) {
  const depth = useRef(0);
  const [dragging, setDragging] = useState(false);
  const reset = () => { depth.current = 0; setDragging(false); };
  useEffect(() => {
    // Capture drop so attachment handlers may stop bubbling without stranding feedback.
    window.addEventListener("drop", reset, true);
    window.addEventListener("dragend", reset);
    window.addEventListener("blur", reset);
    return () => {
      window.removeEventListener("drop", reset, true);
      window.removeEventListener("dragend", reset);
      window.removeEventListener("blur", reset);
    };
  }, []);
  return {
    dragging,
    onDragEnter(event: DragEvent) {
      if (!hasFiles(event)) return;
      depth.current += 1; setDragging(true);
    },
    onDragLeave() {
      depth.current = Math.max(0, depth.current - 1);
      if (!depth.current) setDragging(false);
    },
    onDragOver(event: DragEvent) {
      if (hasFiles(event)) event.preventDefault();
    },
    onDrop(event: DragEvent) {
      reset();
      if (!hasFiles(event)) return;
      event.preventDefault(); onFiles(Array.from(event.dataTransfer.files));
    },
  };
}
