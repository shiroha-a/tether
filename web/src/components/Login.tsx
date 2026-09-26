import { useState } from "react";
import { api, setToken } from "../api";

/** Token entry screen shown when the server requires authentication. */
export default function Login({ onDone }: { onDone: () => void }) {
  const [value, setValue] = useState("");
  const [error, setError] = useState("");

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setToken(value.trim());
    try {
      await api.config();
      onDone();
    } catch {
      setToken("");
      setError("トークンが正しくありません");
    }
  };

  return (
    <div className="login">
      <form onSubmit={submit} className="login-card">
        <img src="/icon.svg" alt="" width={56} height={56} />
        <h1>tether</h1>
        <label>
          <span>アクセストークン</span>
          <input
            type="password"
            autoComplete="current-password"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            autoFocus
          />
        </label>
        {error && <p className="error">{error}</p>}
        <button className="btn primary" disabled={!value.trim()}>
          ログイン
        </button>
      </form>
    </div>
  );
}
