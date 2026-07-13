// Preview tab: anything listening on a guest TCP port, one URL away —
// rendered in an iframe through the WebSocket-transparent guest proxy.
// Auth rides an HttpOnly proxy-session cookie (lib/proxy.ts).

import { useEffect, useMemo, useRef, useState } from "react";
import type { Sandbox } from "../../api/types";
import { IconGlobe, IconRefresh } from "../../components/icons";
import { Tip } from "../../components/tooltip";
import { Button, Empty, ErrorNote, IconButton, Mono, inputCls } from "../../components/ui";
import { useI18n } from "../../lib/i18n";
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
  const { t } = useI18n();
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
          <p>{t("Preview needs a running guest.", "预览需要运行中的 guest。")}</p>
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
            {t("port", "端口")}
          </label>
          <input
            className={`${inputCls} w-24 font-mono`}
            value={portInput}
            onChange={(e) => setPortInput(e.target.value.replace(/\D/g, ""))}
            placeholder="8080"
            inputMode="numeric"
            aria-label={t("Guest port", "guest 端口")}
          />
          <label className="font-mono text-[10px] uppercase tracking-[0.14em] text-faint">
            {t("path", "路径")}
          </label>
          <input
            className={`${inputCls} w-40 font-mono`}
            value={path}
            onChange={(e) => setPath(e.target.value)}
            aria-label={t("Path", "路径")}
          />
          <Button size="sm" kind="primary" type="submit" disabled={!portInput}>
            {t("Open", "打开")}
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
          {/* No "open in new tab": the proxy is same-origin with the console,
              and a top-level tab is NOT covered by the iframe sandbox — guest
              code there would run at the console's origin and could read the
              bearer token from localStorage. Preview stays inside the
              opaque-origin sandboxed iframe only. */}
          <Tip content={t("Reload preview", "刷新预览")}>
            <IconButton
              label={t("Reload", "刷新")}
              onClick={() => setGeneration((g) => g + 1)}
              disabled={!src}
            >
              <IconRefresh size={13} />
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
                {t(
                  "Anything listening inside the guest is one URL away.",
                  "guest 里任何监听的服务，都只有一个 URL 之遥。",
                )}
              </p>
              <p className="text-faint">
                {t(
                  "Start a server in the Terminal tab, then open its port here — for example:",
                  "在「终端」标签里起一个服务，然后在这里打开它的端口 —— 例如：",
                )}
              </p>
              <pre className="rounded-md border border-hairline bg-surface p-3 font-mono text-xs text-ink">
                python3 -m http.server 8000
              </pre>
              <p className="text-faint">
                {t("The proxy is WebSocket-transparent, so dev servers with HMR work too. External REST clients can hit", "代理对 WebSocket 透明，因此带 HMR 的开发服务器也能用。外部 REST 客户端可携带 bearer 令牌访问")}{" "}
                <Mono className="text-ink">/v0/sandboxes/:id/proxy/:port/…</Mono>
                {t(" with a bearer token.", "。")}
              </p>
            </div>
          </Empty>
        )}
        {src && (
          <iframe
            key={`${port}#${generation}`}
            ref={iframeRef}
            src={src}
            title={`${t("Guest port", "guest 端口")} ${port}`}
            className="h-full w-full border-0 bg-white"
            // The proxy is same-origin with the console, so allow-same-origin
            // would give guest-served (untrusted, agent-controlled) scripts
            // the console's origin — i.e. read of the bearer token in
            // localStorage and reach into the parent DOM. Omitting it forces
            // an opaque origin: scripts still run (preview works, HMR
            // WebSockets included — those are URL-based network requests), but
            // they cannot touch the console. Downloads are likewise dropped
            // (they would inherit the origin).
            sandbox="allow-scripts allow-forms"
          />
        )}
      </div>
    </div>
  );
}
