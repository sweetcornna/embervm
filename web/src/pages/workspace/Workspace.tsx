// Workspace shell: full-bleed per-sandbox IDE surface. Header (breadcrumb,
// state, live health pill, verbs) + route-driven tabs. Tab state IS the URL
// (#/sandboxes/:id/<tab>) so every pane deep-links and survives refresh.

import { Suspense, lazy, useEffect } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { fmtPct } from "../../api/client";
import { useSandbox, useSandboxAction, verbs } from "../../api/hooks";
import type { Sandbox, SandboxState } from "../../api/types";
import { Sparkline } from "../../components/charts";
import {
  IconBranch,
  IconCamera,
  IconDots,
  IconPause,
  IconPlay,
  IconTerminal,
} from "../../components/icons";
import { Menu, MenuItem, MenuSeparator } from "../../components/menu";
import { StateBadge } from "../../components/status";
import { TabBar } from "../../components/tabs";
import { Tip } from "../../components/tooltip";
import {
  Button,
  ConfirmDialog,
  ErrorNote,
  Mono,
  Skeleton,
  Spinner,
  useConfirm,
} from "../../components/ui";
import { useSandboxHealth } from "../../lib/health";
import { useI18n } from "../../lib/i18n";
import { disposeTermSandbox, noteTermState } from "../../lib/termBridge";
import { toast, toastError } from "../../lib/toast";
import { CheckpointsTab } from "./CheckpointsTab";
import { OverviewTab } from "./OverviewTab";
import { PreviewTab } from "./PreviewTab";
import { SettingsTab } from "./SettingsTab";

// xterm.js and CodeMirror stay out of the entry chunk; these tabs load on
// first use.
const TerminalTab = lazy(() =>
  import("./TerminalTab").then((m) => ({ default: m.TerminalTab })),
);
const FilesTab = lazy(() => import("./FilesTab").then((m) => ({ default: m.FilesTab })));

const TABS = [
  { value: "overview", label: "Overview", zh: "总览" },
  { value: "terminal", label: "Terminal", zh: "终端" },
  { value: "files", label: "Files", zh: "文件" },
  { value: "preview", label: "Preview", zh: "预览" },
  { value: "checkpoints", label: "Checkpoints", zh: "检查点" },
  { value: "settings", label: "Settings", zh: "设置" },
] as const;

type TabValue = (typeof TABS)[number]["value"];

export function Workspace() {
  const params = useParams();
  const id = params.id ?? "";
  const rawTab = (params["*"] ?? "").split("/")[0];
  const tab: TabValue = (TABS.some((x) => x.value === rawTab) ? rawTab : "overview") as TabValue;
  const nav = useNavigate();
  const { t } = useI18n();
  const { data: sb, isLoading, error } = useSandbox(id);

  // Terminal reconnection is gated on lifecycle state.
  useEffect(() => {
    if (sb) noteTermState(sb.id, sb.state);
  }, [sb]);

  if (isLoading)
    return (
      <div className="space-y-4 p-6">
        <Skeleton className="h-7 w-72" />
        <Skeleton className="h-9 w-full max-w-lg" />
        <Skeleton className="h-40 w-full" />
      </div>
    );
  if (error || !sb)
    return (
      <div className="space-y-3 p-6">
        <ErrorNote error={error ?? new Error(t("sandbox not found", "未找到沙箱"))} />
        <Link to="/sandboxes" className="text-[13px] text-accent hover:underline">
          {t("← back to sandboxes", "← 返回沙箱列表")}
        </Link>
      </div>
    );

  return (
    <div className="flex h-full min-h-0 flex-col">
      <WorkspaceHeader sb={sb} />
      <TabBar
        tabs={TABS.map((tb) => ({ value: tb.value, label: t(tb.label, tb.zh) }))}
        value={tab}
        onChange={(v) => nav(`/sandboxes/${id}/${v === "overview" ? "" : v}`)}
      />
      <StateBanner sb={sb} tab={tab} />
      <div className="min-h-0 flex-1 overflow-hidden">
        {tab === "overview" && (
          <div className="h-full overflow-y-auto">
            <OverviewTab sb={sb} />
          </div>
        )}
        {(tab === "terminal" || tab === "files") && (
          <Suspense
            fallback={
              <div className="grid h-full place-items-center text-muted">
                <Spinner />
              </div>
            }
          >
            {tab === "terminal" && <TerminalTab sb={sb} />}
            {tab === "files" && <FilesTab sb={sb} />}
          </Suspense>
        )}
        {tab === "preview" && <PreviewTab sb={sb} />}
        {tab === "checkpoints" && (
          <div className="h-full overflow-y-auto">
            <CheckpointsTab sb={sb} />
          </div>
        )}
        {tab === "settings" && (
          <div className="h-full overflow-y-auto">
            <SettingsTab sb={sb} />
          </div>
        )}
      </div>
    </div>
  );
}

function WorkspaceHeader(props: { sb: Sandbox }) {
  const { sb } = props;
  const nav = useNavigate();
  const confirm = useConfirm();
  const { t } = useI18n();

  const pause = useSandboxAction(() => verbs.pause(sb.id), {
    sandboxId: sb.id,
    optimistic: () => ({ state: "PAUSING" as SandboxState }),
    onError: toastError(t("Pause failed", "暂停失败")),
  });
  const resume = useSandboxAction(() => verbs.resume(sb.id), {
    sandboxId: sb.id,
    optimistic: () => ({ state: "RESUMING" as SandboxState }),
    onError: toastError(t("Resume failed", "恢复失败")),
  });
  const snapshot = useSandboxAction(() => verbs.snapshot(sb.id, "console"), {
    sandboxId: sb.id,
    onSuccess: () =>
      toast.success(t("Snapshot taken", "已快照"), t("pause → checkpoint → resume", "暂停 → 检查点 → 恢复")),
    onError: toastError(t("Snapshot failed", "快照失败")),
  });
  const fork = useSandboxAction(() => verbs.fork(sb.id), {
    sandboxId: sb.id,
    onSuccess: (child) => {
      nav(`/sandboxes/${child.id}`);
      toast.action("success", `${t("Forked to", "已派生到")} ${child.id.slice(0, 8)}`, {
        label: t("Back to parent", "返回父沙箱"),
        onClick: () => nav(`/sandboxes/${sb.id}`),
      });
    },
    onError: toastError(t("Fork failed", "派生失败")),
  });
  const migrate = useSandboxAction(() => verbs.migrate(sb.id), {
    sandboxId: sb.id,
    onSuccess: (moved) =>
      toast.success(t("Migrated", "已迁移"), `${t("now on node", "现在位于节点")} ${moved.node_id ?? "?"}`),
    onError: toastError(t("Migrate failed", "迁移失败")),
  });
  const kill = useSandboxAction(() => verbs.kill(sb.id), {
    onSuccess: () => {
      disposeTermSandbox(sb.id);
      toast.success(`${t("Sandbox", "沙箱")} ${sb.id.slice(0, 8)} ${t("destroyed", "已销毁")}`);
      nav("/sandboxes");
    },
    onError: toastError(t("Kill failed", "销毁失败")),
  });

  const running = sb.state === "RUNNING";
  const pausedLike =
    sb.state.startsWith("PAUSED") || sb.state === "ARCHIVED_COLD" || sb.state === "FAILED";

  return (
    <header className="flex flex-wrap items-center gap-x-3 gap-y-2 border-b border-hairline px-4 py-2.5">
      <div className="flex min-w-0 items-center gap-3">
        <h1 className="truncate text-[15px] font-semibold tracking-tight">
          <Link to="/sandboxes" className="text-faint transition-colors hover:text-muted">
            {t("Sandboxes", "沙箱")}
          </Link>
          <span className="mx-1.5 text-faint">/</span>
          <Mono className="text-[14px]">{sb.id.slice(0, 8)}</Mono>
        </h1>
        <StateBadge state={sb.state} />
        <HealthPill sb={sb} />
      </div>
      <div className="ml-auto flex items-center gap-1.5">
        {running ? (
          <Button size="sm" onClick={() => pause.mutate()} busy={pause.isPending}>
            <IconPause size={13} /> {t("Pause", "暂停")}
          </Button>
        ) : (
          <Button
            size="sm"
            kind="primary"
            onClick={() => resume.mutate()}
            busy={resume.isPending}
            disabled={!pausedLike}
          >
            <IconPlay size={13} /> {t("Resume", "恢复")}
          </Button>
        )}
        <Tip content={t("Pause → checkpoint → resume", "暂停 → 检查点 → 恢复")}>
          <Button size="sm" onClick={() => snapshot.mutate()} busy={snapshot.isPending} disabled={!running}>
            <IconCamera size={13} /> {t("Snapshot", "快照")}
          </Button>
        </Tip>
        <Tip content={t("Checkpoint now, branch a new sandbox from it", "立即检查点，从中派生新沙箱")}>
          <Button size="sm" onClick={() => fork.mutate()} busy={fork.isPending} disabled={!running}>
            <IconBranch size={13} /> {t("Fork", "派生")}
          </Button>
        </Tip>
        <Menu
          trigger={
            <button
              aria-label={t("More actions", "更多操作")}
              className="inline-flex size-7 items-center justify-center rounded-md text-muted hover:bg-raised hover:text-ink"
            >
              <IconDots />
            </button>
          }
        >
          <MenuItem
            onSelect={() => migrate.mutate()}
            disabled={!running && sb.state !== "PAUSED_HOT"}
          >
            {t("Migrate to another node", "迁移到其他节点")}
          </MenuItem>
          <MenuItem
            onSelect={() => {
              void navigator.clipboard.writeText(sb.id);
              toast.info(t("Sandbox id copied", "已复制沙箱 id"));
            }}
          >
            {t("Copy sandbox id", "复制沙箱 id")}
          </MenuItem>
          <MenuSeparator />
          <MenuItem danger onSelect={() => confirm.ask(() => kill.mutate())}>
            {t("Kill sandbox…", "销毁沙箱…")}
          </MenuItem>
        </Menu>
      </div>
      <ConfirmDialog
        open={confirm.open}
        title={t("Kill sandbox", "销毁沙箱")}
        body={
          <>
            {t("Destroy ", "永久销毁 ")}
            <Mono className="text-ink">{sb.id.slice(0, 8)}</Mono>
            {t(
              " permanently? Its disk, checkpoints and snapshots are deleted. Sandboxes with live forks are protected server-side.",
              "？其磁盘、检查点与快照都会被删除。存在活动派生的沙箱在服务端受保护。",
            )}
          </>
        }
        confirmLabel={t("Kill sandbox", "销毁沙箱")}
        busy={kill.isPending}
        onConfirm={confirm.confirm}
        onClose={confirm.close}
      />
    </header>
  );
}

/** Always-visible pressure readout: an operator never loses sight of the
    guest while on another tab. Colored by pressure, detail on hover. */
function HealthPill(props: { sb: Sandbox }) {
  const { latest, samples, unreachable } = useSandboxHealth(props.sb.id);
  const { t } = useI18n();
  if (props.sb.state !== "RUNNING") return null;
  if (unreachable)
    return (
      <span className="rounded-full border border-danger/35 bg-danger/10 px-2 py-0.5 font-mono text-[11px] text-danger">
        {t("guest unreachable", "guest 不可达")}
      </span>
    );
  const h = latest?.health;
  if (!h?.ok || !h.mem_total_kib) return null;
  const memUsed = 1 - (h.mem_available_kib ?? 0) / h.mem_total_kib;
  const psi = Math.max(h.psi_mem_some10 ?? 0, h.psi_cpu_some10 ?? 0);
  const level =
    memUsed > 0.9 || psi > 25
      ? "var(--color-danger)"
      : memUsed > 0.8 || psi > 10
        ? "var(--color-transit)"
        : "var(--color-ok)";
  return (
    <Tip
      mono
      content={
        <div className="w-44 space-y-1 py-0.5">
          <div className="flex justify-between">
            <span className="text-muted">{t("mem used", "内存占用")}</span>
            <span>{fmtPct(memUsed)}</span>
          </div>
          <div className="flex justify-between">
            <span className="text-muted">{t("psi mem·cpu", "psi 内存·cpu")}</span>
            <span>
              {(h.psi_mem_some10 ?? 0).toFixed(1)} · {(h.psi_cpu_some10 ?? 0).toFixed(1)}
            </span>
          </div>
          <Sparkline
            points={samples
              .filter((s) => s.health.ok && s.health.mem_total_kib)
              .map((s) => ({
                at: s.at,
                value: 1 - (s.health.mem_available_kib ?? 0) / (s.health.mem_total_kib ?? 1),
              }))}
            label={t("memory used", "内存占用")}
            format={fmtPct}
            yMin={0}
            yMax={1}
          />
        </div>
      }
    >
      <span
        className="inline-flex cursor-default items-center gap-1.5 rounded-full border px-2 py-0.5 font-mono text-[11px] tabular-nums"
        style={{
          color: level,
          borderColor: `color-mix(in srgb, ${level} 35%, transparent)`,
          background: `color-mix(in srgb, ${level} 10%, transparent)`,
        }}
      >
        mem {fmtPct(memUsed)} · psi {psi.toFixed(1)}
      </span>
    </Tip>
  );
}

/** Non-RUNNING states get one honest banner instead of dead panes. */
function StateBanner(props: { sb: Sandbox; tab: TabValue }) {
  const { sb } = props;
  const { t } = useI18n();
  if (sb.state === "RUNNING") return null;
  const resumable =
    sb.state.startsWith("PAUSED") || sb.state === "ARCHIVED_COLD" || sb.state === "FAILED";
  const message: Record<string, string> = {
    PAUSED_HOT: t("Paused · hot — resume is sub-second.", "已暂停 · 热 —— 恢复在亚秒级。"),
    PAUSED_WARM: t(
      "Paused · warm — state lives in the chunk store; resume restores it.",
      "已暂停 · 温 —— 状态存于分块存储；恢复时还原。",
    ),
    ARCHIVED_COLD: t(
      "Archived · cold — resume rehydrates from the cold store.",
      "已归档 · 冷 —— 恢复时从冷存储重新加载。",
    ),
    FAILED: sb.error
      ? `${t("Failed", "失败")}: ${sb.error}`
      : t("Failed — resume retries from the last snapshot.", "失败 —— 恢复将从上一个快照重试。"),
    RECYCLED: t("Recycled — only extracted artifacts remain.", "已回收 —— 仅保留提取的产物。"),
    STOPPED: t("Stopped.", "已停止。"),
  };
  return (
    <div className="flex flex-wrap items-center gap-3 border-b border-hairline bg-surface px-4 py-2 text-[13px] text-muted">
      <IconTerminal size={14} />
      <span className="min-w-0">
        {message[sb.state] ?? `${sb.state} — ${t("hold on…", "请稍候…")}`}
        {resumable &&
          t(
            " Terminal, files and live gauges need a running guest.",
            " 终端、文件与实时仪表都需要运行中的 guest。",
          )}
      </span>
    </div>
  );
}
