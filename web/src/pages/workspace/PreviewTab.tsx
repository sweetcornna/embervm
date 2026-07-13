// Preview tab: anything listening on a guest TCP port, one URL away —
// rendered in an iframe through the WebSocket-transparent guest proxy.
// Auth rides an HttpOnly proxy-session cookie (lib/proxy.ts).

import { useEffect, useMemo, useRef, useState } from "react";
import type { Sandbox } from "../../api/types";
import { IconExternal, IconGlobe, IconRefresh } from "../../components/icons";
import { Tip } from "../../components/tooltip";
import { Button, Empty, ErrorNote, IconButton, Mono, inputCls } from "../../components/ui";
import { ensureProxySession, proxyURL } from "../../lib/proxy";

const QUICK_PORTS = [3000, 5173, 8000, 8080];

function recentKey(id: string) {
  return `embervm.ports.${id}`;
}

function loadRecent(id: string): number[] {
  try {
    const raw = JSON.parse(localStorage.getItem(recentKey(id)) ?? "[]");
    return Array.isArray(raw) ? raw.filter((n) => Number.isInteger(n)) : [];
  } catch {
    return [];
  }
}

export function PreviewTab(props: { sb: Sandbox }) {
  const { sb } = props;
  const running = sb.state === "RUNNING";
  const [port, setPort] = useState<number | null>(() => loadRecent(sb.id)[0] ?? null);
  const [portInput, setPortInput] = useState(port ? String(port) : "");
  const [path, setPath] = useState("/");
  const [sessionReady, setSessionReady] = useState(false);
  const [sessionErr, setSessionErr] = useState<Error | null>(null);
  const [generation, setGeneration] = useState(0); // bump to reload the iframe
  const iframeRef = useRef<HTMLIFrameElement>(null);

  useEffect(() => {
    ensureProxySession()
      .then(() => setSessionReady(true))
      .catch((err) => setSessionErr(err as Error));
  }, []);

  const recent = useMemo(() => loadRecent(sb.id), [sb.id]);

  const open = (p: number) => {
    if (!Number.isInteger(p) || p <= 0 || p > 65535) return;
    setPort(p);
    setPortInput(String(p));
    setGeneration((g) => g + 1);
    const next = [p, ...loadRecent(sb.id).filter((x) => x !== p)].slice(0, 5);
    localStorage.setItem(recentKey(sb.id), JSON.stringify(next));
  };

  const src = port && sessionReady ? proxyURL(sb.id, port, path) : null;

  if (!running)
    return (
      <Empty>
        <div className="mx-auto max-w-sm space-y-2">
          <IconGlobe size={22} className="mx-auto text-faint" />
          <p>Preview needs a running guest.</p>
        </div>
      </Empty>
    );

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex flex-wrap items-center gap-2 border-b border-hairline px-3 py-1.5">
        <form
          className="flex items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            open(Number(portInput));
          }}
        >
          <label className="font-mono text-[10px] uppercase tracking-[0.14em] text-faint">
            port
          </label>
          <input
            className={`${inputCls} w-24 font-mono`}
            value={portInput}
            onChange={(e) => setPortInput(e.target.value.replace(/\D/g, ""))}
            placeholder="8080"
            inputMode="numeric"
            aria-label="Guest port"
          />
          <label className="font-mono text-[10px] uppercase tracking-[0.14em] text-faint">
            path
          </label>
          <input
            className={`${inputCls} w-40 font-mono`}
            value={path}
            onChange={(e) => setPath(e.target.value)}
            aria-label="Path"
          />
          <Button size="sm" kind="primary" type="submit" disabled={!portInput}>
            Open
          </Button>
        </form>
        <div className="flex items-center gap-1">
          {[...new Set([...recent, ...QUICK_PORTS])].slice(0, 6).map((p) => (
            <button
              key={p}
              onClick={() => open(p)}
              className={`rounded-full border px-2 py-0.5 font-mono text-[11px] transition-colors ${
                p === port
                  ? "border-accent/50 bg-accent-weak text-accent"
                  : "border-border text-muted hover:border-accent/40 hover:text-ink"
              }`}
            >
              {p}
            </button>
          ))}
        </div>
        <div className="ml-auto flex items-center">
          <Tip content="Reload preview">
            <IconButton label="Reload" onClick={() => setGeneration((g) => g + 1)} disabled={!src}>
              <IconRefresh size={13} />
            </IconButton>
          </Tip>
          <Tip content="Open in a new tab">
            <IconButton
              label="Open in new tab"
              onClick={() => src && window.open(src, "_blank", "noopener")}
              disabled={!src}
            >
              <IconExternal size={13} />
            </IconButton>
          </Tip>
        </div>
      </div>
      <div className="min-h-0 flex-1 bg-bg">
        {sessionErr && (
          <div className="p-4">
            <ErrorNote error={sessionErr} />
          </div>
        )}
        {!port && !sessionErr && (
          <Empty>
            <div className="mx-auto max-w-md space-y-3 text-left">
              <p className="text-center">
                Anything listening inside the guest is one URL away.
              </p>
              <p className="text-faint">
                Start a server in the Terminal tab, then open its port here — for example:
              </p>
              <pre className="rounded-md border border-hairline bg-surface p-3 font-mono text-xs text-ink">
                python3 -m http.server 8000
              </pre>
              <p className="text-faint">
                The proxy is WebSocket-transparent, so dev servers with HMR work too. External
                REST clients can hit{" "}
                <Mono className="text-ink">/v0/sandboxes/:id/proxy/:port/…</Mono> with a bearer
                token.
              </p>
            </div>
          </Empty>
        )}
        {src && (
          <iframe
            key={`${port}#${generation}`}
            ref={iframeRef}
            src={src}
            title={`Guest port ${port}`}
            className="h-full w-full border-0 bg-white"
            sandbox="allow-scripts allow-forms allow-same-origin allow-downloads"
          />
        )}
      </div>
    </div>
  );
}
