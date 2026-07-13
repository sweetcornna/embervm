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
import { disposeTermSandbox, noteTermState } from "../../lib/termBridge";
import { toast, toastError } from "../../lib/toast";
import { CheckpointsTab } from "./CheckpointsTab";
import { OverviewTab } from "./OverviewTab";

// xterm.js and CodeMirror stay out of the entry chunk; these tabs load on
// first use.
const TerminalTab = lazy(() =>
  import("./TerminalTab").then((m) => ({ default: m.TerminalTab })),
);
const FilesTab = lazy(() => import("./FilesTab").then((m) => ({ default: m.FilesTab })));

const TABS = [
  { value: "overview", label: "Overview" },
  { value: "terminal", label: "Terminal" },
  { value: "files", label: "Files" },
  { value: "checkpoints", label: "Checkpoints" },
] as const;

type TabValue = (typeof TABS)[number]["value"];

export function Workspace() {
  const params = useParams();
  const id = params.id ?? "";
  const rawTab = (params["*"] ?? "").split("/")[0];
  const tab: TabValue = (TABS.some((t) => t.value === rawTab) ? rawTab : "overview") as TabValue;
  const nav = useNavigate();
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
        <ErrorNote error={error ?? new Error("sandbox not found")} />
        <Link to="/sandboxes" className="text-[13px] text-accent hover:underline">
          ← back to sandboxes
        </Link>
      </div>
    );

  return (
    <div className="flex h-full min-h-0 flex-col">
      <WorkspaceHeader sb={sb} />
      <TabBar
        tabs={TABS.map((t) => ({ ...t }))}
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
        {tab === "checkpoints" && (
          <div className="h-full overflow-y-auto">
            <CheckpointsTab sb={sb} />
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

  const pause = useSandboxAction(() => verbs.pause(sb.id), {
    sandboxId: sb.id,
    optimistic: () => ({ state: "PAUSING" as SandboxState }),
    onError: toastError("Pause failed"),
  });
  const resume = useSandboxAction(() => verbs.resume(sb.id), {
    sandboxId: sb.id,
    optimistic: () => ({ state: "RESUMING" as SandboxState }),
    onError: toastError("Resume failed"),
  });
  const snapshot = useSandboxAction(() => verbs.snapshot(sb.id, "console"), {
    sandboxId: sb.id,
    onSuccess: () => toast.success("Snapshot taken", "pause → checkpoint → resume"),
    onError: toastError("Snapshot failed"),
  });
  const fork = useSandboxAction(() => verbs.fork(sb.id), {
    sandboxId: sb.id,
    onSuccess: (child) => {
      nav(`/sandboxes/${child.id}`);
      toast.action("success", `Forked to ${child.id.slice(0, 8)}`, {
        label: "Back to parent",
        onClick: () => nav(`/sandboxes/${sb.id}`),
      });
    },
    onError: toastError("Fork failed"),
  });
  const migrate = useSandboxAction(() => verbs.migrate(sb.id), {
    sandboxId: sb.id,
    onSuccess: (moved) => toast.success("Migrated", `now on node ${moved.node_id ?? "?"}`),
    onError: toastError("Migrate failed"),
  });
  const kill = useSandboxAction(() => verbs.kill(sb.id), {
    onSuccess: () => {
      disposeTermSandbox(sb.id);
      toast.success(`Sandbox ${sb.id.slice(0, 8)} destroyed`);
      nav("/sandboxes");
    },
    onError: toastError("Kill failed"),
  });

  const running = sb.state === "RUNNING";
  const pausedLike =
    sb.state.startsWith("PAUSED") || sb.state === "ARCHIVED_COLD" || sb.state === "FAILED";

  return (
    <header className="flex flex-wrap items-center gap-x-3 gap-y-2 border-b border-hairline px-4 py-2.5">
      <div className="flex min-w-0 items-center gap-3">
        <h1 className="truncate text-[15px] font-semibold tracking-tight">
          <Link to="/sandboxes" className="text-faint transition-colors hover:text-muted">
            Sandboxes
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
            <IconPause size={13} /> Pause
          </Button>
        ) : (
          <Button
            size="sm"
            kind="primary"
            onClick={() => resume.mutate()}
            busy={resume.isPending}
            disabled={!pausedLike}
          >
            <IconPlay size={13} /> Resume
          </Button>
        )}
        <Tip content="Pause → checkpoint → resume">
          <Button size="sm" onClick={() => snapshot.mutate()} busy={snapshot.isPending} disabled={!running}>
            <IconCamera size={13} /> Snapshot
          </Button>
        </Tip>
        <Tip content="Checkpoint now, branch a new sandbox from it">
          <Button size="sm" onClick={() => fork.mutate()} busy={fork.isPending} disabled={!running}>
            <IconBranch size={13} /> Fork
          </Button>
        </Tip>
        <Menu
          trigger={
            <button
              aria-label="More actions"
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
            Migrate to another node
          </MenuItem>
          <MenuItem
            onSelect={() => {
              void navigator.clipboard.writeText(sb.id);
              toast.info("Sandbox id copied");
            }}
          >
            Copy sandbox id
          </MenuItem>
          <MenuSeparator />
          <MenuItem danger onSelect={() => confirm.ask(() => kill.mutate())}>
            Kill sandbox…
          </MenuItem>
        </Menu>
      </div>
      <ConfirmDialog
        open={confirm.open}
        title="Kill sandbox"
        body={
          <>
            Destroy <Mono className="text-ink">{sb.id.slice(0, 8)}</Mono> permanently? Its disk,
            checkpoints and snapshots are deleted. Sandboxes with live forks are protected server-side.
          </>
        }
        confirmLabel="Kill sandbox"
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
  if (props.sb.state !== "RUNNING") return null;
  if (unreachable)
    return (
      <span className="rounded-full border border-danger/35 bg-danger/10 px-2 py-0.5 font-mono text-[11px] text-danger">
        guest unreachable
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
            <span className="text-muted">mem used</span>
            <span>{fmtPct(memUsed)}</span>
          </div>
          <div className="flex justify-between">
            <span className="text-muted">psi mem·cpu</span>
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
            label="memory used"
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
  if (sb.state === "RUNNING") return null;
  const resumable =
    sb.state.startsWith("PAUSED") || sb.state === "ARCHIVED_COLD" || sb.state === "FAILED";
  const message: Record<string, string> = {
    PAUSED_HOT: "Paused · hot — resume is sub-second.",
    PAUSED_WARM: "Paused · warm — state lives in the chunk store; resume restores it.",
    ARCHIVED_COLD: "Archived · cold — resume rehydrates from the cold store.",
    FAILED: sb.error ? `Failed: ${sb.error}` : "Failed — resume retries from the last snapshot.",
    RECYCLED: "Recycled — only extracted artifacts remain.",
    STOPPED: "Stopped.",
  };
  return (
    <div className="flex flex-wrap items-center gap-3 border-b border-hairline bg-surface px-4 py-2 text-[13px] text-muted">
      <IconTerminal size={14} />
      <span className="min-w-0">
        {message[sb.state] ?? `${sb.state} — hold on…`}
        {resumable && " Terminal, files and live gauges need a running guest."}
      </span>
    </div>
  );
}
