import type { MouseEvent, ReactNode } from "react";
import { extractPaths, type KnownPath } from "../paths";

/** Plain text with known file paths rendered as links that open a preview. */
export default function PathText({
  text,
  known,
  onOpen,
}: {
  text: string;
  known: Map<string, KnownPath>;
  onOpen: (p: KnownPath) => void;
}) {
  const matches = known.size ? extractPaths(text).filter((m) => known.has(m.path)) : [];
  if (matches.length === 0) return <>{text}</>;
  const parts: ReactNode[] = [];
  let pos = 0;
  for (const m of matches) {
    const k = known.get(m.path)!;
    parts.push(text.slice(pos, m.start));
    parts.push(
      <a
        key={m.start}
        className="file-link"
        href="#"
        onClick={(e: MouseEvent) => {
          // detailsの見出しの中でも、開閉ではなくプレビューを開く
          e.preventDefault();
          e.stopPropagation();
          onOpen(k);
        }}
      >
        {text.slice(m.start, m.end)}
      </a>,
    );
    pos = m.end;
  }
  parts.push(text.slice(pos));
  return <>{parts}</>;
}
