import { useState, type RefObject } from "react";
import type { Annotation, ReaderSelection } from "@neoread/core";
import { Button } from "@neoread/ui/components/button";
import { Textarea } from "@neoread/ui/components/textarea";
import { Check, Sparkles } from "lucide-react";
import { AnnotationNoteWindow } from "./AnnotationNoteWindow";
import { AnnotationQuote } from "./AnnotationQuote";

export function AnnotationNoteForm({
  open,
  onOpenChange,
  selection,
  kind,
  annotation,
  saving,
  canAnswer,
  draft,
  onSave,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  selection: ReaderSelection | null;
  kind: "note" | "question";
  annotation: Annotation | null;
  saving: boolean;
  canAnswer: boolean;
  draft: RefObject<string>;
  onSave: (text: string, answer: boolean) => void;
}) {
  const [note, setNote] = useState(() => annotation?.note ?? "");
  // The tab-close guard reads the latest draft without rendering Workspace.
  draft.current = note;
  return (
    <AnnotationNoteWindow
      open={open}
      anchor={selection?.anchor}
      title={
        kind === "question"
          ? annotation
            ? "编辑问题"
            : "提问"
          : annotation?.note.trim()
            ? "编辑批注"
            : "添加批注"
      }
      description={
        kind === "question" ? "就当前选区提问" : "为当前选区添加批注"
      }
      onOpenChange={onOpenChange}
    >
      <blockquote className="note-preview max-h-24 overflow-y-auto">
        {selection && (
          <AnnotationQuote
            quote={selection.text}
            location={selection.location}
          />
        )}
      </blockquote>
      <Textarea
        autoFocus
        placeholder={kind === "question" ? "想弄清楚什么？" : "批注内容…"}
        value={note}
        onChange={(e) => setNote(e.target.value)}
      />
      {kind === "question" ? (
        <div className="flex gap-2">
          <Button
            variant="outline"
            className="flex-1"
            onClick={() => onSave(note, false)}
            disabled={saving || !note.trim()}
          >
            <Check />
            保存问题
          </Button>
          {!annotation && (
            <Button
              className="flex-1"
              disabled={saving || !note.trim() || !canAnswer}
              onClick={() => onSave(note, true)}
            >
              <Sparkles />
              保存并让 AI 回答
            </Button>
          )}
        </div>
      ) : (
        <Button
          onClick={() => onSave(note, false)}
          disabled={saving || (!note.trim() && !annotation?.note.trim())}
        >
          <Check />
          保存批注
        </Button>
      )}
    </AnnotationNoteWindow>
  );
}
