// File-browser support: guest path helpers, editor guards (binary sniff,
// size ceilings), language detection, and the raw read/write calls.

import { apiRaw } from "../api/client";

/** Join + normalize an absolute guest path (guests are always Linux). */
export function joinPath(dir: string, name: string): string {
  const joined = dir.endsWith("/") ? dir + name : `${dir}/${name}`;
  return normalizePath(joined);
}

export function normalizePath(p: string): string {
  const parts = p.split("/");
  const out: string[] = [];
  for (const part of parts) {
    if (part === "" || part === ".") continue;
    if (part === "..") out.pop();
    else out.push(part);
  }
  return "/" + out.join("/");
}

export function parentPath(p: string): string {
  const n = normalizePath(p);
  if (n === "/") return "/";
  return n.slice(0, n.lastIndexOf("/")) || "/";
}

export function baseName(p: string): string {
  const n = normalizePath(p);
  return n.slice(n.lastIndexOf("/") + 1);
}

/* Editor guards. Over WARN the editor opens read-only; over MAX it refuses
   and offers download — a 1 GiB paste into CodeMirror helps nobody. */
export const EDIT_WARN_BYTES = 2 * 1024 * 1024;
export const EDIT_MAX_BYTES = 8 * 1024 * 1024;

/** NUL in the first 8 KiB — the classic "this is not text" heuristic. */
export function looksBinary(bytes: Uint8Array): boolean {
  const n = Math.min(bytes.length, 8192);
  for (let i = 0; i < n; i++) {
    if (bytes[i] === 0) return true;
  }
  return false;
}

/** CodeMirror language id by file name; "" = plain text. */
export function languageOf(name: string): string {
  const base = name.toLowerCase();
  if (base === "dockerfile" || base.startsWith("dockerfile.")) return "dockerfile";
  if (base === "makefile") return "shell";
  const ext = base.includes(".") ? base.slice(base.lastIndexOf(".") + 1) : "";
  const map: Record<string, string> = {
    js: "javascript",
    mjs: "javascript",
    cjs: "javascript",
    jsx: "javascript",
    ts: "javascript",
    tsx: "javascript",
    py: "python",
    json: "json",
    md: "markdown",
    markdown: "markdown",
    html: "html",
    htm: "html",
    css: "css",
    yaml: "yaml",
    yml: "yaml",
    sh: "shell",
    bash: "shell",
    zsh: "shell",
    env: "shell",
    go: "go",
    toml: "toml",
    txt: "",
    log: "",
    conf: "",
    ini: "",
  };
  return map[ext] ?? "";
}

export async function readGuestFile(
  sandboxId: string,
  path: string,
): Promise<Uint8Array> {
  const resp = await apiRaw(
    "GET",
    `/sandboxes/${sandboxId}/files?path=${encodeURIComponent(path)}`,
  );
  return new Uint8Array(await resp.arrayBuffer());
}

export async function writeGuestFile(
  sandboxId: string,
  path: string,
  content: string | Uint8Array | Blob,
): Promise<void> {
  const body: BodyInit =
    typeof content === "string"
      ? new Blob([content])
      : content instanceof Uint8Array
        ? new Blob([content.slice().buffer])
        : content;
  await apiRaw(
    "PUT",
    `/sandboxes/${sandboxId}/files?path=${encodeURIComponent(path)}`,
    body,
    "application/octet-stream",
  );
}

/** Browser-download a guest file via a blob URL (auth rides the fetch). */
export async function downloadGuestFile(sandboxId: string, path: string) {
  const bytes = await readGuestFile(sandboxId, path);
  const url = URL.createObjectURL(new Blob([bytes.slice().buffer]));
  const a = document.createElement("a");
  a.href = url;
  a.download = baseName(path);
  a.click();
  URL.revokeObjectURL(url);
}
