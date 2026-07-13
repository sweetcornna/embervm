// Minimal typed client for the /v0 REST surface. The bearer token lives in
// localStorage; a 401 clears it and the router falls back to the login page.

const TOKEN_KEY = "embervm.token";

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY);
}

export function setToken(token: string) {
  localStorage.setItem(TOKEN_KEY, token);
}

export function clearToken() {
  localStorage.removeItem(TOKEN_KEY);
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

export async function api<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = {
    Authorization: `Bearer ${getToken() ?? ""}`,
  };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const resp = await fetch(`/v0${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (resp.status === 401) {
    clearToken();
    window.dispatchEvent(new Event("embervm:unauthorized"));
    throw new ApiError(401, "invalid or expired token");
  }
  if (resp.status === 204) return undefined as T;
  const text = await resp.text();
  let data: unknown;
  try {
    data = text ? JSON.parse(text) : undefined;
  } catch {
    data = undefined;
  }
  if (!resp.ok) {
    const msg =
      data && typeof data === "object" && "error" in data
        ? String((data as { error: unknown }).error)
        : `HTTP ${resp.status}`;
    throw new ApiError(resp.status, msg);
  }
  return data as T;
}

/** Raw-body request for the /files endpoints (bytes, not JSON). Returns the
    Response so callers stream or .arrayBuffer() as needed. */
export async function apiRaw(
  method: string,
  path: string,
  body?: BodyInit,
  contentType?: string,
): Promise<Response> {
  const headers: Record<string, string> = {
    Authorization: `Bearer ${getToken() ?? ""}`,
  };
  if (contentType) headers["Content-Type"] = contentType;
  const resp = await fetch(`/v0${path}`, { method, headers, body });
  if (resp.status === 401) {
    clearToken();
    window.dispatchEvent(new Event("embervm:unauthorized"));
    throw new ApiError(401, "invalid or expired token");
  }
  if (!resp.ok) {
    let msg = `HTTP ${resp.status}`;
    try {
      const data = (await resp.json()) as { error?: unknown };
      if (data && data.error) msg = String(data.error);
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(resp.status, msg);
  }
  return resp;
}

/** base64url (no padding) — the token encoding for WS subprotocol auth
    (browser WebSocket cannot set an Authorization header). */
export function b64url(s: string): string {
  return btoa(String.fromCharCode(...new TextEncoder().encode(s)))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
}

/** Decode a Go []byte JSON field (base64) into text for display. */
export function decodeBytes(b64?: string): string {
  if (!b64) return "";
  try {
    return new TextDecoder().decode(
      Uint8Array.from(atob(b64), (c) => c.charCodeAt(0)),
    );
  } catch {
    return b64;
  }
}

export function fmtMiB(mib: number): string {
  return mib >= 1024 && mib % 256 === 0
    ? `${(mib / 1024).toFixed(mib % 1024 === 0 ? 0 : 2)} GiB`
    : `${mib} MiB`;
}

export function fmtBytes(n: number): string {
  if (n <= 0) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  const i = Math.min(units.length - 1, Math.floor(Math.log2(n) / 10));
  const v = n / 2 ** (10 * i);
  return `${v >= 100 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
}

export function fmtAge(iso: string): string {
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return `${Math.floor(s)}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

export function fmtKiB(kib: number): string {
  return fmtBytes(kib * 1024);
}

export function fmtPct(fraction: number): string {
  const pct = fraction * 100;
  return `${pct >= 10 ? pct.toFixed(0) : pct.toFixed(1)}%`;
}
