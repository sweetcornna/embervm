// TermManager: module-level owner of interactive terminal sessions. Owning
// them OUTSIDE React means scrollback, the live WebSocket, and the shell
// survive tab switches and route changes — TerminalTab only re-parents each
// session's container <div> on mount. Wire protocol (guestapi): binary
// frames = raw PTY bytes, text frames = JSON control, auth = bearer token in
// a Sec-WebSocket-Protocol entry (browser WS cannot set headers).

import { FitAddon } from "@xterm/addon-fit";
import { WebLinksAddon } from "@xterm/addon-web-links";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { useCallback, useSyncExternalStore } from "react";
import { b64url, getToken } from "../api/client";
import type { SandboxState, TermControl } from "../api/types";
import { TERM_SUBPROTOCOL } from "../api/types";

export type SessionStatus = "connecting" | "open" | "reconnecting" | "closed";

export interface TermSession {
  id: string;
  sandboxId: string;
  title: string;
  term: Terminal;
  fit: FitAddon;
  /** Detached DOM node the Terminal renders into; the tab re-parents it. */
  container: HTMLDivElement;
  status: SessionStatus;
  exitCode?: number;
}

/** Injectable for unit tests (protocol handling without a server). */
export type WSFactory = (url: string, protocols: string[]) => WebSocket;
let wsFactory: WSFactory = (url, protocols) => new WebSocket(url, protocols);
export function setWSFactory(f: WSFactory) {
  wsFactory = f;
}

// The terminal must look native to the console: colors come from the same
// slate/ember system as the design tokens (index.css @theme).
const TERM_THEME = {
  background: "#0c0e13", // --color-bg
  foreground: "#e7eaf0", // --color-ink
  cursor: "#f5a524", // --color-accent
  cursorAccent: "#0c0e13",
  selectionBackground: "#f5a52445",
  black: "#12151c",
  red: "#e5534b",
  green: "#3fb454",
  yellow: "#d1a01f",
  blue: "#4a94dd",
  magenta: "#b07dd6",
  cyan: "#3fb0b4",
  white: "#9aa3b2",
  brightBlack: "#616b7c",
  brightRed: "#f3a6a2",
  brightGreen: "#7ed492",
  brightYellow: "#f5c95c",
  brightBlue: "#8abdf0",
  brightMagenta: "#d3aef0",
  brightCyan: "#7ed4d8",
  brightWhite: "#e7eaf0",
};

const MAX_SESSIONS = 6;
const BACKOFF_MIN_MS = 500;
const BACKOFF_MAX_MS = 8_000;

interface Internal {
  session: TermSession;
  ws: WebSocket | null;
  userClosed: boolean;
  shellExited: boolean;
  backoffMs: number;
  retryTimer: ReturnType<typeof setTimeout> | null;
  disposeTerm: () => void;
}

const bySandbox = new Map<string, Internal[]>();
const lastState = new Map<string, SandboxState>();
const listeners = new Set<() => void>();
// Immutable per-sandbox snapshots for useSyncExternalStore.
const snapshots = new Map<string, TermSession[]>();
const EMPTY: TermSession[] = [];
let seq = 1;

function publish(sandboxId: string) {
  const list = bySandbox.get(sandboxId) ?? [];
  snapshots.set(
    sandboxId,
    list.map((i) => ({ ...i.session })),
  );
  for (const l of listeners) l();
}

function wsURL(sandboxId: string, cols: number, rows: number): string {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${location.host}/v0/sandboxes/${sandboxId}/term?cols=${cols}&rows=${rows}`;
}

function connect(i: Internal) {
  const { session } = i;
  const token = getToken();
  if (!token) return;
  session.status = i.backoffMs > BACKOFF_MIN_MS ? "reconnecting" : "connecting";
  publish(session.sandboxId);

  const ws = wsFactory(
    wsURL(session.sandboxId, session.term.cols, session.term.rows),
    [`bearer.${b64url(token)}`, TERM_SUBPROTOCOL],
  );
  ws.binaryType = "arraybuffer";
  i.ws = ws;

  ws.onopen = () => {
    i.backoffMs = BACKOFF_MIN_MS;
    session.status = "open";
    publish(session.sandboxId);
  };
  ws.onmessage = (ev) => {
    if (typeof ev.data === "string") {
      try {
        const ctl = JSON.parse(ev.data) as TermControl;
        if (ctl.type === "exit") {
          i.shellExited = true;
          session.exitCode = ctl.code ?? 0;
        }
      } catch {
        /* not a control frame */
      }
      return;
    }
    session.term.write(new Uint8Array(ev.data as ArrayBuffer));
  };
  ws.onclose = () => {
    if (i.ws !== ws) return; // superseded by a newer connection
    i.ws = null;
    if (i.userClosed) return;
    if (i.shellExited) {
      session.status = "closed";
      session.term.write(
        `\r\n\x1b[2m[process exited: ${session.exitCode ?? 0}]\x1b[0m\r\n`,
      );
      publish(session.sandboxId);
      return;
    }
    // Connection lost (pause, migrate, network): retry while RUNNING.
    session.status = "reconnecting";
    publish(session.sandboxId);
    scheduleReconnect(i);
  };
}

function scheduleReconnect(i: Internal) {
  if (i.retryTimer || i.userClosed || i.shellExited) return;
  const jitter = Math.random() * 0.3 + 0.85;
  const delay = Math.min(i.backoffMs * jitter, BACKOFF_MAX_MS);
  i.backoffMs = Math.min(i.backoffMs * 2, BACKOFF_MAX_MS);
  i.retryTimer = setTimeout(() => {
    i.retryTimer = null;
    if (i.userClosed || i.shellExited || i.ws) return;
    if (lastState.get(i.session.sandboxId) !== "RUNNING") {
      scheduleReconnect(i); // keep waiting; noteState() short-circuits this
      return;
    }
    connect(i);
  }, delay);
}

function newSession(sandboxId: string): Internal {
  const term = new Terminal({
    fontFamily: 'jetbrains mono variable, "JetBrains Mono", monospace',
    fontSize: 13,
    lineHeight: 1.25,
    cursorBlink: true,
    scrollback: 10_000,
    theme: TERM_THEME,
    allowProposedApi: true,
  });
  const fit = new FitAddon();
  term.loadAddon(fit);
  term.loadAddon(new WebLinksAddon());

  const container = document.createElement("div");
  container.className = "h-full w-full";
  term.open(container);

  const session: TermSession = {
    id: `sh-${seq++}`,
    sandboxId,
    title: `sh ${seq - 1}`,
    term,
    fit,
    container,
    status: "connecting",
  };
  const i: Internal = {
    session,
    ws: null,
    userClosed: false,
    shellExited: false,
    backoffMs: BACKOFF_MIN_MS,
    retryTimer: null,
    disposeTerm: () => term.dispose(),
  };

  const enc = new TextEncoder();
  term.onData((data) => {
    if (i.ws?.readyState === WebSocket.OPEN) i.ws.send(enc.encode(data));
  });
  term.onResize(({ cols, rows }) => {
    if (i.ws?.readyState === WebSocket.OPEN) {
      i.ws.send(JSON.stringify({ type: "resize", cols, rows } satisfies TermControl));
    }
  });
  // Let the browser keep copy (with selection) and paste; everything else
  // flows to the PTY.
  term.attachCustomKeyEventHandler((ev) => {
    const mod = ev.metaKey || ev.ctrlKey;
    if (mod && ev.key === "c" && term.hasSelection()) return false;
    if (mod && ev.key === "v") return false;
    return true;
  });
  return i;
}

export const TermManager = {
  sessionsOf(sandboxId: string): TermSession[] {
    return snapshots.get(sandboxId) ?? EMPTY;
  },

  open(sandboxId: string): TermSession | null {
    const list = bySandbox.get(sandboxId) ?? [];
    if (list.length >= MAX_SESSIONS) return null;
    const i = newSession(sandboxId);
    bySandbox.set(sandboxId, [...list, i]);
    connect(i);
    publish(sandboxId);
    return i.session;
  },

  /** Restart the shell of an exited session in place (same scrollback). */
  restart(sandboxId: string, sessionId: string) {
    const i = bySandbox.get(sandboxId)?.find((x) => x.session.id === sessionId);
    if (!i || i.ws || !i.shellExited) return;
    i.shellExited = false;
    i.userClosed = false;
    i.session.exitCode = undefined;
    i.backoffMs = BACKOFF_MIN_MS;
    connect(i);
  },

  close(sandboxId: string, sessionId: string) {
    const list = bySandbox.get(sandboxId) ?? [];
    const i = list.find((x) => x.session.id === sessionId);
    if (!i) return;
    i.userClosed = true;
    if (i.retryTimer) clearTimeout(i.retryTimer);
    i.ws?.close(1000, "user closed");
    i.disposeTerm();
    bySandbox.set(
      sandboxId,
      list.filter((x) => x !== i),
    );
    publish(sandboxId);
  },

  /** The sandbox query feeds state changes in; a resume short-circuits any
      pending reconnect backoff. */
  noteState(sandboxId: string, state: SandboxState) {
    const prev = lastState.get(sandboxId);
    lastState.set(sandboxId, state);
    if (prev !== "RUNNING" && state === "RUNNING") {
      for (const i of bySandbox.get(sandboxId) ?? []) {
        if (i.retryTimer) {
          clearTimeout(i.retryTimer);
          i.retryTimer = null;
        }
        if (!i.ws && !i.userClosed && !i.shellExited) connect(i);
      }
    }
  },

  /** Kill/teardown: drop every session of a sandbox. */
  disposeSandbox(sandboxId: string) {
    for (const i of bySandbox.get(sandboxId) ?? []) {
      i.userClosed = true;
      if (i.retryTimer) clearTimeout(i.retryTimer);
      i.ws?.close(1000, "sandbox gone");
      i.disposeTerm();
    }
    bySandbox.delete(sandboxId);
    lastState.delete(sandboxId);
    publish(sandboxId);
  },

  disposeAll() {
    for (const id of [...bySandbox.keys()]) TermManager.disposeSandbox(id);
  },
};

export function useTermSessions(sandboxId: string): TermSession[] {
  const sub = useCallback((l: () => void) => {
    listeners.add(l);
    return () => listeners.delete(l);
  }, []);
  const get = useCallback(
    () => snapshots.get(sandboxId) ?? EMPTY,
    [sandboxId],
  );
  return useSyncExternalStore(sub, get);
}

window.addEventListener("embervm:unauthorized", () => TermManager.disposeAll());
