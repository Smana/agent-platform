// SPDX-License-Identifier: Apache-2.0

import MarkdownIt, { type Token } from "markdown-it";
import type { RoomEvent } from "./conn";

// T10: room text is untrusted. It is parsed once, HTML off, and its tokens become DOM
// nodes whose text is set as text: no HTML string is built from it, so nothing is
// unescaped twice, and markup SP3's sanitiser defused (&lt;, !\[, ]\:) stays text.
const md = new MarkdownIt({ html: false, linkify: true });

// The elements a token may open; any other opens a span.
const blocks = new Set(["p", "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol", "li", "blockquote", "strong", "em",
  "s", "table", "thead", "tbody", "tr", "th", "td"]);

function el(tag: string, cls: string, text?: string): HTMLElement {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

// A link leaves only for the web: markdown-it already refuses javascript:, vbscript:,
// file: and data:, and a relative link would stay on the broker's origin.
function web(href: string | null): string | null {
  try {
    const u = new URL(href ?? "");
    return u.protocol === "https:" || u.protocol === "http:" ? u.href : null;
  } catch {
    return null;
  }
}

function open(t: Token): HTMLElement {
  if (t.type === "link_open") {
    const href = web(t.attrGet("href"));
    if (!href) return el("span", "");
    const a = el("a", "") as HTMLAnchorElement;
    a.href = href;
    a.rel = "noopener noreferrer nofollow";
    a.target = "_blank";
    return a;
  }
  const e = el(blocks.has(t.tag) ? t.tag : "span", "");
  const start = Number(t.attrGet("start"));
  if (t.type === "ordered_list_open" && Number.isInteger(start) && start >= 0) (e as HTMLOListElement).start = start;
  return e;
}

function leaf(t: Token, parent: HTMLElement) {
  switch (t.type) {
    case "inline":
      build(t.children ?? [], parent);
      return;
    case "code_inline":
      parent.append(el("code", "", t.content));
      return;
    case "code_block":
    case "fence": {
      const pre = el("pre", "");
      pre.append(el("code", "", t.content));
      parent.append(pre);
      return;
    }
    case "softbreak":
      parent.append("\n");
      return;
    case "hardbreak":
      parent.append(el("br", ""));
      return;
    case "hr":
      parent.append(el("hr", ""));
      return;
    case "image": // never loaded (img-src 'self' would block it anyway): its alt text shows
      parent.append(`[image: ${md.renderer.renderInlineAsText(t.children ?? [], md.options, {})}]`);
      return;
    default: // text, and anything else markdown-it may emit, as text
      if (t.content) parent.append(t.content);
  }
}

function build(tokens: Token[], root: HTMLElement) {
  const stack = [root];
  for (const t of tokens) {
    const top = stack[stack.length - 1];
    if (t.nesting === 1) {
      const e = open(t);
      top.append(e);
      stack.push(e);
    } else if (t.nesting === -1) {
      if (stack.length > 1) stack.pop();
    } else {
      leaf(t, top);
    }
  }
}

// markdown renders untrusted markdown into a fresh element.
export function markdown(src: string): HTMLElement {
  const body = el("div", "md");
  build(md.parse(src, {}), body);
  return body;
}

// A class name from a payload value: letters, digits, _ and - only.
const cls = (v: unknown) => String(v ?? "").replace(/[^A-Za-z0-9_-]/g, "");

export function renderEvent(ev: RoomEvent): HTMLElement {
  const row = el("article", `ev ev-${cls(ev.type)}`);
  row.dataset.seq = String(ev.seq);
  row.append(el("header", "ev-head",
    `#${ev.seq} · ${ev.actor.id}${ev.actor.role ? " (" + ev.actor.role + ")" : ""} · ${new Date(ev.ts).toLocaleTimeString()}`));
  const p = ev.payload ?? {};
  switch (ev.type) {
    case "message":
      if (p.kind === "review_verdict") row.append(el("div", `verdict verdict-${cls(p.verdict)}`, `verdict: ${p.verdict}`));
      row.append(markdown(String(p.text ?? "")));
      break;
    case "tool_call": {
      const d = el("details", "tool");
      d.append(el("summary", "", `${p.tool} ${p.risk ? "· risk " + p.risk : ""}`), el("pre", "", JSON.stringify(p.args, null, 2)));
      row.append(d);
      break;
    }
    case "tool_result": {
      const d = el("details", `tool-result status-${cls(p.status)}`);
      d.append(el("summary", "", `${p.status}${p.truncated ? " · truncated (" + p.bytes + " B)" : ""}`), el("pre", "", String(p.output ?? "")));
      row.append(d);
      break;
    }
    case "state_changed":
      row.append(el("div", "state", [p.kind, p.phase ?? p.status ?? "", p.reason ?? ""].filter(Boolean).join(" · ")));
      break;
    default:
      row.append(el("pre", "raw", JSON.stringify(p, null, 2)));
  }
  if (ev.redactions?.length) row.append(el("div", "redacted", `redacted: ${ev.redactions.join(", ")}`));
  return row;
}
