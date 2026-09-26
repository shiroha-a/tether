import { useEffect, useRef, type ReactNode } from "react";

/** Minimal accessible modal dialog closed by Escape or backdrop click. */
export function Modal({
  title,
  onClose,
  children,
  wide,
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
  wide?: boolean;
}) {
  const dialogRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      // ターミナル（xterm）は処理したキーの伝播を止めるので、先に受け取れるキャプチャ段階で処理する。
      // ここで止めないと、閉じるためのEscがClaude Codeにも送られて作業を中断させてしまう
      e.preventDefault();
      e.stopPropagation();
      onClose();
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [onClose]);

  // 開いたらダイアログにフォーカスを移し（キー入力をターミナルに渡さない）、閉じたら元に戻す
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    dialogRef.current?.focus();
    return () => previous?.focus?.();
  }, []);

  return (
    <div className="modal-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div
        ref={dialogRef}
        tabIndex={-1}
        className={"modal" + (wide ? " wide" : "")}
        role="dialog"
        aria-modal="true"
        aria-label={title}
      >
        <header className="modal-head">
          <h2>{title}</h2>
          <button className="icon-btn" onClick={onClose} aria-label="閉じる">
            ✕
          </button>
        </header>
        <div className="modal-body">{children}</div>
      </div>
    </div>
  );
}
