import { useMemo, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { fmtAge } from "../api/client";
import { useSandboxAction, useSandboxes, verbs } from "../api/hooks";
import type { Sandbox, SandboxState } from "../api/types";
import { CreateSandboxDialog } from "../components/createSandbox";
import { IconChevronDown, IconDots } from "../components/icons";
import { Menu, MenuItem, MenuSeparator } from "../components/menu";
import { MemGauge, StateBadge, stateLabel } from "../components/status";
import {
  Button,
  ConfirmDialog,
  Empty,
  Mono,
  PageHeader,
  Skeleton,
  Table,
  inputCls,
} from "../components/ui";
import { useI18n } from "../lib/i18n";
import { disposeTermSandbox } from "../lib/termBridge";
import { toast, toastError } from "../lib/toast";

type SortKey = "state" | "memory" | "age";

// A stable order for state grouping in the sort.
const STATE_RANK: SandboxState[] = [
  "RUNNING",
  "STARTING",
  "RESUMING",
  "PENDING",
  "PAUSING",
  "PAUSED_HOT",
  "PAUSED_WARM",
  "ARCHIVED_COLD",
  "STOPPING",
  "STOPPED",
  "RECYCLED",
  "FAILED",
];

export function Sandboxes() {
  const { t } = useI18n();
  const { data, isLoading } = useSandboxes();
  const [creating, setCreating] = useState(false);
  const [query, setQuery] = useState("");
  const [filter, setFilter] = useState<SandboxState | "all">("all");
  const [sort, setSort] = useState<{ key: SortKey; dir: 1 | -1 }>({ key: "age", dir: -1 });

  const all = data ?? [];
  const counts = useMemo(() => {
    const m = new Map<SandboxState, number>();
    for (const sb of all) m.set(sb.state, (m.get(sb.state) ?? 0) + 1);
    return m;
  }, [all]);

  const rows = useMemo(() => {
    const q = query.trim().toLowerCase();
    let r = all.filter((sb) => {
      if (filter !== "all" && sb.state !== filter) return false;
      if (!q) return true;
      return (
        sb.id.toLowerCase().includes(q) ||
        sb.template_id.toLowerCase().includes(q) ||
        (sb.node_id ?? "").toLowerCase().includes(q)
      );
    });
    r = [...r].sort((a, b) => {
      let c = 0;
      if (sort.key === "state") c = STATE_RANK.indexOf(a.state) - STATE_RANK.indexOf(b.state);
      else if (sort.key === "memory") c = a.memory_mib - b.memory_mib;
      else c = new Date(a.created_at).getTime() - new Date(b.created_at).getTime();
      return c * sort.dir;
    });
    return r;
  }, [all, query, filter, sort]);

  const toggleSort = (key: SortKey) =>
    setSort((s) => (s.key === key ? { key, dir: s.dir === 1 ? -1 : 1 } : { key, dir: 1 }));

  const chips: Array<SandboxState | "all"> = [
    "all",
    ...STATE_RANK.filter((s) => (counts.get(s) ?? 0) > 0),
  ];

  return (
    <div className="space-y-5">
      <PageHeader
        title={t("Sandboxes", "沙箱")}
        subtitle={all.length > 0 ? `${all.length} ${t("total", "总计")}` : undefined}
        actions={
          <Button kind="primary" onClick={() => setCreating(true)}>
            {t("New sandbox", "新建沙箱")}
          </Button>
        }
      />

      <div className="flex flex-wrap items-center gap-3">
        <input
          className={`${inputCls} max-w-xs`}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder={t("Filter by id, template, node…", "按 id、模板、节点筛选…")}
          aria-label={t("Filter sandboxes", "筛选沙箱")}
        />
        <div className="flex flex-wrap gap-1">
          {chips.map((c) => (
            <button
              key={c}
              onClick={() => setFilter(c)}
              className={`rounded-full border px-2.5 py-0.5 text-xs font-medium transition-colors ${
                filter === c
                  ? "border-accent/50 bg-accent-weak text-accent"
                  : "border-border text-muted hover:border-accent/40 hover:text-ink"
              }`}
            >
              {c === "all" ? `${t("All", "全部")} ${all.length}` : `${stateLabel(c, t)} ${counts.get(c) ?? 0}`}
            </button>
          ))}
        </div>
      </div>

      <Table
        head={[
          <SortHeader key="s" label={t("State", "状态")} active={sort.key === "state"} dir={sort.dir} onClick={() => toggleSort("state")} />,
          t("Sandbox", "沙箱"),
          <SortHeader key="m" label={t("Memory", "内存")} active={sort.key === "memory"} dir={sort.dir} onClick={() => toggleSort("memory")} />,
          t("vCPUs", "vCPU"),
          t("Node", "节点"),
          <SortHeader key="a" label={t("Age", "时长")} active={sort.key === "age"} dir={sort.dir} onClick={() => toggleSort("age")} />,
          "",
        ]}
      >
        {rows.map((sb) => (
          <Row key={sb.id} sb={sb} />
        ))}
      </Table>
      {isLoading && (
        <div className="space-y-2">
          <Skeleton className="h-10 w-full" />
          <Skeleton className="h-10 w-full" />
        </div>
      )}
      {!isLoading && all.length === 0 && (
        <Empty>
          {t(
            "No sandboxes. “New sandbox” boots one from a READY template.",
            "暂无沙箱。点击“新建沙箱”从 READY 模板启动一个。",
          )}
        </Empty>
      )}
      {!isLoading && all.length > 0 && rows.length === 0 && (
        <Empty>{t("No sandboxes match this filter.", "没有沙箱匹配此筛选。")}</Empty>
      )}

      <CreateSandboxDialog open={creating} onClose={() => setCreating(false)} />
    </div>
  );
}

function SortHeader(props: { label: string; active: boolean; dir: 1 | -1; onClick: () => void }) {
  return (
    <button
      onClick={props.onClick}
      className={`inline-flex items-center gap-1 ${props.active ? "text-muted" : ""}`}
      aria-sort={props.active ? (props.dir === 1 ? "ascending" : "descending") : "none"}
    >
      {props.label}
      {props.active && (
        <IconChevronDown
          size={11}
          className={props.dir === 1 ? "rotate-180 transition-transform" : "transition-transform"}
        />
      )}
    </button>
  );
}

function Row(props: { sb: Sandbox }) {
  const { sb } = props;
  const { t } = useI18n();
  const nav = useNavigate();
  const [confirmKill, setConfirmKill] = useState(false);
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
  const fork = useSandboxAction(() => verbs.fork(sb.id), {
    sandboxId: sb.id,
    onSuccess: (child) => nav(`/sandboxes/${child.id}`),
    onError: toastError(t("Fork failed", "派生失败")),
  });
  const kill = useSandboxAction(() => verbs.kill(sb.id), {
    onSuccess: () => {
      disposeTermSandbox(sb.id);
      toast.success(`${t("Sandbox", "沙箱")} ${sb.id.slice(0, 8)} ${t("destroyed", "已销毁")}`);
    },
    onError: toastError(t("Kill failed", "销毁失败")),
  });
  const running = sb.state === "RUNNING";
  const pausedLike = sb.state.startsWith("PAUSED") || sb.state === "ARCHIVED_COLD" || sb.state === "FAILED";

  return (
    <tr className="border-b border-hairline last:border-0 hover:bg-raised/40">
      <td className="px-4 py-2.5">
        <StateBadge state={sb.state} />
      </td>
      <td className="px-4 py-2.5">
        <Link to={`/sandboxes/${sb.id}`} className="hover:text-accent">
          <Mono>{sb.id.slice(0, 8)}</Mono>
        </Link>
        <div className="flex gap-2 font-mono text-[11px] text-faint">
          {sb.autoscale && <span className="text-transit">autoscale</span>}
          {sb.forked_from && <span>fork:{sb.forked_from}</span>}
        </div>
      </td>
      <td className="px-4 py-2.5">
        <MemGauge state={sb.state} memoryMiB={sb.memory_mib} baseMiB={sb.base_memory_mib} maxMiB={sb.max_memory_mib} />
      </td>
      <td className="px-4 py-2.5">
        <Mono className="tabular-nums">
          {sb.vcpus}
          {(sb.max_vcpus ?? 0) > 0 && <span className="text-faint"> / {sb.max_vcpus}</span>}
        </Mono>
      </td>
      <td className="px-4 py-2.5">
        <Mono className="text-muted">{sb.node_id || "—"}</Mono>
      </td>
      <td className="px-4 py-2.5">
        <Mono className="text-muted tabular-nums">{fmtAge(sb.created_at)}</Mono>
      </td>
      <td className="px-2 py-2.5 text-right">
        <Menu
          trigger={
            <button
              aria-label={`${t("Actions for", "操作")} ${sb.id.slice(0, 8)}`}
              className="inline-flex size-7 items-center justify-center rounded-md text-muted hover:bg-raised hover:text-ink"
            >
              <IconDots />
            </button>
          }
        >
          <MenuItem onSelect={() => nav(`/sandboxes/${sb.id}`)}>{t("Open workspace", "打开工作区")}</MenuItem>
          {running && <MenuItem onSelect={() => pause.mutate()}>{t("Pause", "暂停")}</MenuItem>}
          {pausedLike && <MenuItem onSelect={() => resume.mutate()}>{t("Resume", "恢复")}</MenuItem>}
          <MenuItem onSelect={() => fork.mutate()} disabled={!running}>
            {t("Fork", "派生")}
          </MenuItem>
          <MenuSeparator />
          <MenuItem danger onSelect={() => setConfirmKill(true)}>
            {t("Kill…", "销毁…")}
          </MenuItem>
        </Menu>
        <ConfirmDialog
          open={confirmKill}
          title={t("Kill sandbox", "销毁沙箱")}
          body={
            <>
              {t("Destroy", "销毁")} <Mono className="text-ink">{sb.id.slice(0, 8)}</Mono>
              {t("? Its disk, checkpoints and snapshots are deleted.", "？其磁盘、检查点和快照都将被删除。")}
            </>
          }
          confirmLabel={t("Kill sandbox", "销毁沙箱")}
          busy={kill.isPending}
          onConfirm={() => {
            kill.mutate();
            setConfirmKill(false);
          }}
          onClose={() => setConfirmKill(false)}
        />
      </td>
    </tr>
  );
}
