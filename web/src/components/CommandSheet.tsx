import { useEffect, useState } from "react";
import { api, type Snippet } from "../api";

interface Props {
  /** Bumped when another device changes the list. */
  version: number;
  /** True while the shell cannot take input (disconnected or exited). */
  disabled: boolean;
  onRun: (command: string) => void;
  onClose: () => void;
}

/** Sheet over the terminal listing saved commands; tapping one runs it. */
export default function CommandSheet({ version, disabled, onRun, onClose }: Props) {
  const [items, setItems] = useState<Snippet[] | null>(null);
  const [editing, setEditing] = useState(false);
  // 変更中の項目のid。nullなら新規追加
  const [target, setTarget] = useState<string | null>(null);
  const [label, setLabel] = useState("");
  const [command, setCommand] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    let alive = true;
    api
      .snippets()
      .then((list) => {
        if (!alive) return;
        setItems(list);
        // 何も登録されていなければ、最初から追加フォームを出す
        if (list.length === 0) setEditing(true);
      })
      .catch((e) => alive && setError((e as Error).message));
    return () => {
      alive = false;
    };
  }, [version]);

  const resetForm = () => {
    setTarget(null);
    setLabel("");
    setCommand("");
  };

  const save = async () => {
    setBusy(true);
    setError("");
    try {
      const saved = target ? await api.updateSnippet(target, label, command) : await api.addSnippet(label, command);
      setItems((prev) => {
        const list = prev ?? [];
        return target ? list.map((it) => (it.id === saved.id ? saved : it)) : [...list, saved];
      });
      resetForm();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const remove = async (it: Snippet) => {
    setError("");
    try {
      await api.deleteSnippet(it.id);
      setItems((prev) => (prev ?? []).filter((x) => x.id !== it.id));
      if (target === it.id) resetForm();
    } catch (e) {
      setError((e as Error).message);
    }
  };

  const startEdit = (it: Snippet) => {
    setTarget(it.id);
    setLabel(it.label);
    setCommand(it.command);
  };

  return (
    <div className="copy-sheet command-sheet" role="dialog" aria-label="よく使うコマンド">
      <div className="copy-head">
        <strong className="small">よく使うコマンド</strong>
        <div className="command-head-actions">
          <button
            className={"btn small" + (editing ? " primary" : "")}
            onClick={() => {
              setEditing((v) => !v);
              resetForm();
              setError("");
            }}
          >
            {editing ? "完了" : "編集"}
          </button>
          <button className="icon-btn" onClick={onClose} aria-label="閉じる">
            ✕
          </button>
        </div>
      </div>
      <div className="command-body">
        {items === null && !error && <p className="muted small">読み込み中...</p>}
        {items?.length === 0 && (
          <p className="muted small">
            {editing
              ? "よく使うコマンドを下の欄から登録すると、ここからボタン1つで実行できます。"
              : "まだ登録されていません。「編集」から追加できます。"}
          </p>
        )}
        {!editing && disabled && items && items.length > 0 && (
          <p className="muted small">シェルに接続していないため実行できません。</p>
        )}
        <ul className="command-list">
          {items?.map((it) => (
            <li key={it.id} className={"command-item" + (target === it.id ? " selected" : "")}>
              {editing ? (
                <>
                  <div className="command-text">
                    {it.label && <strong>{it.label}</strong>}
                    <code>{it.command}</code>
                  </div>
                  <button className="btn small" onClick={() => startEdit(it)}>
                    変更
                  </button>
                  <button className="btn small danger" onClick={() => remove(it)}>
                    削除
                  </button>
                </>
              ) : (
                <button
                  className="command-run"
                  disabled={disabled}
                  title={it.command}
                  // ボタンを押してもターミナルからフォーカスを奪わない（スマホでキーボードが閉じるのを防ぐ）
                  onMouseDown={(e) => e.preventDefault()}
                  onClick={() => onRun(it.command)}
                >
                  <span className="command-text">
                    {it.label && <strong>{it.label}</strong>}
                    <code>{it.command}</code>
                  </span>
                  <span className="command-go" aria-hidden="true">
                    ⏎
                  </span>
                </button>
              )}
            </li>
          ))}
        </ul>
        {error && <p className="error">{error}</p>}
      </div>
      {editing && (
        <form
          className="command-form"
          onSubmit={(e) => {
            e.preventDefault();
            if (command.trim()) save();
          }}
        >
          <input
            value={label}
            maxLength={60}
            placeholder="名前（省略可）"
            aria-label="名前"
            onChange={(e) => setLabel(e.target.value)}
          />
          <input
            className="mono"
            value={command}
            placeholder="コマンド（例: git status）"
            aria-label="コマンド"
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            onChange={(e) => setCommand(e.target.value)}
          />
          <div className="command-form-actions">
            {target && (
              <button type="button" className="btn small" onClick={resetForm}>
                キャンセル
              </button>
            )}
            <button type="submit" className="btn small primary" disabled={busy || !command.trim()}>
              {target ? "保存" : "追加"}
            </button>
          </div>
        </form>
      )}
    </div>
  );
}
