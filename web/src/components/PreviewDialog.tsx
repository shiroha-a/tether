import { useEffect, useMemo, useState } from "react";
import { marked } from "marked";
import DOMPurify from "dompurify";
import { neutralizeExternalImages } from "../markdown";
import { api, type FsEntry, type Preview } from "../api";
import { Modal } from "./Modal";

const IMAGE_EXT = /\.(png|jpe?g|gif|webp)$/i;
const HTML_EXT = /\.html?$/i;

/**
 * Rewrites relative image sources in rendered Markdown so they load from the
 * Markdown file's folder through the download API (absolute URLs are left as is).
 */
export function resolveImages(html: string, filePath: string): string {
  const doc = new DOMParser().parseFromString(`<body>${html}</body>`, "text/html");
  const dir = filePath.slice(0, filePath.lastIndexOf("/") + 1);
  for (const img of doc.querySelectorAll("img")) {
    const src = img.getAttribute("src") ?? "";
    // スキーム付き（http:, data:等）とプロトコル相対はそのまま。相対パスとサーバ上の絶対パスだけを置き換える
    if (!src || /^[a-z][a-z0-9+.-]*:/i.test(src) || src.startsWith("//")) continue;
    // new URLはサーバ上の絶対パス（/で始まる）ならそのまま、相対パスならdir基準で解決する
    const abs = new URL(src, "file://" + dir).pathname;
    img.setAttribute("src", api.downloadUrl(decodeURIComponent(abs), true));
  }
  return doc.body.innerHTML;
}

/** Shows a text/markdown/image preview with a download link. */
export default function PreviewDialog({ entry, onClose }: { entry: FsEntry; onClose: () => void }) {
  const isImage = IMAGE_EXT.test(entry.name);
  const isHtml = HTML_EXT.test(entry.name);
  const [preview, setPreview] = useState<Preview | null>(null);
  const [error, setError] = useState("");
  const [raw, setRaw] = useState(false);

  useEffect(() => {
    if (isImage) return;
    api
      .preview(entry.path)
      .then(setPreview)
      .catch((e) => setError(e.status === 415 ? "このファイルはプレビューできません" : String(e.message)));
  }, [entry.path, isImage]);

  const html = useMemo(() => {
    if (!preview || preview.kind !== "markdown" || raw) return "";
    // Markdown内のHTMLはそのまま描画せず、DOMPurifyでスクリプト等を取り除く。
    // 相対パスの画像は同じサイトのダウンロードAPIに向け、外部の画像は読み込まずリンクにする
    const clean = DOMPurify.sanitize(marked.parse(preview.content, { async: false }));
    return neutralizeExternalImages(resolveImages(clean, preview.path));
  }, [preview, raw]);

  return (
    <Modal title={entry.name} onClose={onClose} wide>
      <div className="preview-actions">
        {(preview?.kind === "markdown" || (isHtml && preview)) && (
          <button className="btn" onClick={() => setRaw((v) => !v)}>
            {raw ? (isHtml ? "表示" : "整形表示") : "ソース表示"}
          </button>
        )}
        <a className="btn" href={api.downloadUrl(entry.path)} download>
          ダウンロード
        </a>
      </div>
      {isImage && <img className="preview-image" src={api.downloadUrl(entry.path, true)} alt={entry.name} />}
      {error && <p className="muted">{error}</p>}
      {preview?.truncated && <p className="muted">先頭1MiBだけを表示しています</p>}
      {isHtml && preview && !raw ? (
        // sandboxを空にしてスクリプト・フォーム・同一オリジン扱いをすべて禁止する（tetherのAPIやトークンに触れさせない）。
        // 親ページのCSPも引き継がれるので、外部の画像やスクリプトも読み込まれない
        <iframe className="preview-html" sandbox="" srcDoc={preview.content} title={entry.name} />
      ) : html ? (
        <div className="markdown" dangerouslySetInnerHTML={{ __html: html }} />
      ) : (
        preview && <pre className="preview-text">{preview.content}</pre>
      )}
    </Modal>
  );
}
