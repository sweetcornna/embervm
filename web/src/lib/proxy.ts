// Guest-proxy session plumbing. <iframe> and new-tab navigations cannot
// carry the bearer token, so the console trades it for an HttpOnly cookie
// (POST /v0/proxy-session) that the backend honors on /proxy/ routes only.

import { api } from "../api/client";

let sessionPromise: Promise<void> | null = null;

/** Idempotent: the first caller mints the cookie, everyone else awaits it. */
export function ensureProxySession(): Promise<void> {
  sessionPromise ??= api<void>("POST", "/proxy-session").catch((err) => {
    sessionPromise = null; // allow retry
    throw err;
  });
  return sessionPromise;
}

export function endProxySession() {
  sessionPromise = null;
  void api<void>("DELETE", "/proxy-session").catch(() => {});
}

export function proxyURL(sandboxId: string, port: number, path = "/"): string {
  const p = path.startsWith("/") ? path : `/${path}`;
  return `/v0/sandboxes/${sandboxId}/proxy/${port}${p}`;
}

window.addEventListener("embervm:unauthorized", () => {
  sessionPromise = null;
});
