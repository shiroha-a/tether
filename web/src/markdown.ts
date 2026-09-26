import { marked } from "marked";
import DOMPurify from "dompurify";

// data:やblob:はその場で完結するので外部への送信にならない。それ以外のスキーム付きURLとプロトコル相対URLは外部
const EXTERNAL = /^(?!data:|blob:)([a-z][a-z0-9+.-]*:|\/\/)/i;

/**
 * Replaces remote images in sanitized HTML with links, so viewing a message
 * never makes the browser contact another site (a remote image URL can carry
 * data, e.g. from a prompt-injected reply). The CSP blocks them anyway; this
 * keeps the content readable and lets the user open the image on purpose.
 */
export function neutralizeExternalImages(html: string): string {
  const doc = new DOMParser().parseFromString(`<body>${html}</body>`, "text/html");
  for (const img of doc.querySelectorAll("img")) {
    const src = img.getAttribute("src") ?? "";
    // srcsetは候補URLを別に持てるので、外部かどうかに関係なく使わない
    img.removeAttribute("srcset");
    if (!EXTERNAL.test(src)) continue;
    const a = doc.createElement("a");
    a.setAttribute("href", src);
    a.setAttribute("target", "_blank");
    a.setAttribute("rel", "noopener noreferrer");
    a.textContent = `画像: ${img.getAttribute("alt") || src}`;
    img.replaceWith(a);
  }
  for (const source of doc.querySelectorAll("source")) source.remove();
  return doc.body.innerHTML;
}

/** Renders untrusted Markdown to safe HTML without remote resource loads. */
export function renderMarkdown(text: string): string {
  return neutralizeExternalImages(DOMPurify.sanitize(marked.parse(text, { async: false })));
}
