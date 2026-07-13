// Terminal tab: a session strip over the active xterm. Sessions belong to
// TermManager (module-level) — this component only re-parents the session's
// container div, so scrollback and the live shell survive tab switches.

import { useEffect, useRef, useState } from "react";
import type { Sandbox } from "../../api/types";
import { IconClose, IconPlus, IconTerminal } from "../../components/icons";
import { Tip } from "../../components/tooltip";
import { Button, Empty } from "../../components/ui";
import { useI18n } from "../../lib/i18n";
import type { TermSession } from "../../lib/term";
import { TermManager, useTermSessions } from "../../lib/term";

const STATUS_COLOR: Record<TermSession["status"], string> = {
  connecting: "var(--color-transit)",
  open: "var(--color-ok)",
  reconnecting: "var(--color-transit)",
  closed: "var(--color-idle)",
};

export function TerminalTab(props: { sb: Sandbox }) {
  const { sb } = props;
  const { t } = useI18n();
  const sessions = useTermSessions(sb.id);
  const [activeID, setActiveID] = useState<string | null>(null);
  const active =
    sessions.find((s) => s.id === activeID) ?? sessions[sessions.length - 1] ?? null;
  const running = sb.state === "RUNNING";

  // First visit on a running sandbox: open a shell without asking.
  useEffect(() => {
    if (running && sessions.length === 0) {
      const s = TermManager.open(sb.id);
      if (s) setActiveID(s.id);
    }
  }, [running, sessions.length, sb.id]);

  if (!running && sessions.length === 0)
    return (
      <Empty>
        <div className="mx-auto max-w-sm space-y-2">
          <IconTerminal size={22} className="mx-auto text-faint" />
          <p>{t("The interactive shell needs a running guest.", "交互式 shell 需要运行中的 guest。")}</p>
          <p className="text-faint">
            {t("Resume the sandbox from the header to open a terminal.", "从顶部恢复沙箱即可打开终端。")}
          </p>
        </div>
      </Empty>
    );

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex items-center gap-0.5 border-b border-hairline bg-surface px-2 py-1">
        {sessions.map((s) => (
          <div
            key={s.id}
            className={`group flex items-center rounded-md text-[12px] ${
              s.id === active?.id ? "bg-raised text-ink" : "text-muted hover:bg-raised/60"
            }`}
          >
            <button className="flex items-center gap-1.5 py-1 pl-2.5 pr-1" onClick={() => setActiveID(s.id)}>
              <span
                aria-hidden
                className="size-1.5 rounded-full"
                style={{ background: STATUS_COLOR[s.status] }}
              />
              <span className="font-mono">{s.title}</span>
              {s.status === "reconnecting" && (
                <span className="font-mono text-[10px] text-transit">{t("reconnecting…", "重连中…")}</span>
              )}
            </button>
            <button
              aria-label={`${t("Close", "关闭")} ${s.title}`}
              onClick={() => {
                TermManager.close(sb.id, s.id);
                if (activeID === s.id) setActiveID(null);
              }}
              className="mr-1 rounded p-0.5 text-faint opacity-0 hover:bg-overlay hover:text-ink group-hover:opacity-100"
            >
              <IconClose size={11} />
            </button>
          </div>
        ))}
        <Tip
          content={
            running
              ? t("New shell session", "新建 shell 会话")
              : t("Needs a running guest", "需要运行中的 guest")
          }
        >
          <button
            aria-label={t("New session", "新建会话")}
            disabled={!running}
            onClick={() => {
              const s = TermManager.open(sb.id);
              if (s) setActiveID(s.id);
            }}
            className="ml-0.5 rounded-md p-1.5 text-muted hover:bg-raised hover:text-ink disabled:cursor-not-allowed disabled:opacity-40"
          >
            <IconPlus size={13} />
          </button>
        </Tip>
      </div>
      <div className="relative min-h-0 flex-1 bg-bg">
        {active ? (
          <TermMount key={active.id} session={active} />
        ) : (
          <Empty>{t("No open sessions — start one with the + button.", "暂无会话 —— 用 + 按钮新建。")}</Empty>
        )}
        {active?.status === "closed" && (
          <div className="absolute inset-x-0 bottom-4 flex justify-center">
            <div className="flex items-center gap-3 rounded-md border border-border bg-raised px-3 py-2 text-[12px] text-muted shadow-[var(--shadow-overlay)]">
              {t("process exited", "进程已退出")}
              {active.exitCode !== undefined ? ` (${active.exitCode})` : ""}
              <Button size="sm" onClick={() => TermManager.restart(sb.id, active.id)} disabled={!running}>
                {t("Restart session", "重启会话")}
              </Button>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

function TermMount(props: { session: TermSession }) {
  const hostRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const host = hostRef.current;
    const { container, fit, term } = props.session;
    if (!host) return;
    host.appendChild(container);
    // Fit after layout settles, then follow the pane size.
    const raf = requestAnimationFrame(() => {
      fit.fit();
      term.focus();
    });
    const ro = new ResizeObserver(() => fit.fit());
    ro.observe(host);
    return () => {
      cancelAnimationFrame(raf);
      ro.disconnect();
      // Detach (don't destroy): the session outlives the tab.
      if (container.parentElement === host) host.removeChild(container);
    };
  }, [props.session]);

  return <div ref={hostRef} className="h-full w-full" />;
}
