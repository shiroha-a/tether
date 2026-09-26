import { useEffect, useState } from "react";
import { api, formatDuration, type AboutInfo } from "../api";
import { Modal } from "./Modal";

/** Dialog with the running version, build details and the project repository. */
export default function AboutDialog({ onClose }: { onClose: () => void }) {
  const [info, setInfo] = useState<AboutInfo | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    api
      .about()
      .then(setInfo)
      .catch((e) => setError((e as Error).message));
  }, []);

  const uptime = info ? (Date.now() - Date.parse(info.startedAt)) / 1000 : 0;

  return (
    <Modal title="About tether" onClose={onClose}>
      <div className="about">
        <img src="/icon.svg" alt="" width={64} height={64} />
        <h3>tether</h3>
        <p className="muted">Claude Codeのセッションをブラウザから共有・操作するためのWeb UI</p>
        {error && <p className="error">{error}</p>}
        {info && (
          <>
            <dl className="about-list">
              <dt>バージョン</dt>
              <dd className="mono">{info.version}</dd>
              <dt>Go</dt>
              <dd className="mono">{info.goVersion}</dd>
              <dt>プラットフォーム</dt>
              <dd className="mono">{info.platform}</dd>
              <dt>稼働時間</dt>
              <dd title={new Date(info.startedAt).toLocaleString()}>{formatDuration(uptime)}</dd>
              <dt>ライセンス</dt>
              <dd>{info.license}</dd>
            </dl>
            {info.repository && (
              <a className="btn about-link" href={info.repository} target="_blank" rel="noopener noreferrer">
                GitHubで見る
              </a>
            )}
          </>
        )}
      </div>
    </Modal>
  );
}
