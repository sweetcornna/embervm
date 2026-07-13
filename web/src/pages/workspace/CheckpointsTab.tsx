// Checkpoints tab: the sandbox's history as ONE timeline — lifecycle
// transitions (sandbox_events) merged with checkpoints (fork/rollback
// anchors) — plus the M5 time-travel composer: run a command with a
// checkpoint taken first, so every step is undoable.

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { decodeBytes, fmtAge } from "../../api/client";
import type { ExecResult } from "../../api/hooks";
import { useCheckpoints, useSandboxEvents, verbs } from "../../api/hooks";
import type { Checkpoint, Sandbox, SandboxEvent, SandboxState } from "../../api/types";
import { IconBranch, IconUndo } from "../../components/icons";
import { STATE_META, stateLabel } from "../../components/status";
import { Tip } from "../../components/tooltip";
import {
  Button,
  Card,
  ConfirmDialog,
  Empty,
  ErrorNote,
  Mono,
  Skeleton,
  Toggle,
  inputCls,
} from "../../components/ui";
import { useI18n } from "../../lib/i18n";
import { toast } from "../../lib/toast";

export function CheckpointsTab(props: { sb: Sandbox }) {
  const { sb } = props;
  return (
    <div className="mx-auto max-w-4xl space-y-4 p-4">
      <Composer sb={sb} />
      <Timeline sb={sb} />
    </div>
  );
}

/* ── Time-travel composer ────────────────────────────────────────────── */

function Composer(props: { sb: Sandbox }) {
  const { sb } = props;
  const { t } = useI18n();
  const qc = useQueryClient();
  const nav = useNavigate();
  const [cmdline, setCmdline] = useState("");
  const [withCheckpoint, setWithCheckpoint] = useState(true);
  const [tag, setTag] = useState("");
  const [result, setResult] = useState<ExecResult | null>(null);

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ["sandboxes", sb.id, "checkpoints"] });
    void qc.invalidateQueries({ queryKey: ["sandboxes", sb.id, "events"] });
  };

  const exec = useMutation({
    mutationFn: () => {
      const [cmd, ...args] = cmdline.trim().split(/\s+/);
      return verbs.exec(sb.id, cmd, args, withCheckpoint ? { checkpoint: true } : undefined);
    },
    onSuccess: (out) => {
      setResult(out);
      invalidate();
      if (out.checkpoint) {
        toast.success(`${t("Step checkpointed as", "步骤已检查点为")} ${out.checkpoint}`, t("roll back to undo this step", "回滚即可撤销此步"));
      }
    },
    onError: (err) => toast.error(t("Exec failed", "执行失败"), err.message),
  });
  const checkpoint = useMutation({
    mutationFn: () => verbs.checkpoint(sb.id, tag.trim() || undefined),
    onSuccess: (cp) => {
      setTag("");
      invalidate();
      toast.success(`${t("Checkpoint", "检查点")} ${cp.tag}`, `${t("layer", "层")} ${cp.layer}`);
    },
    onError: (err) => toast.error(t("Checkpoint failed", "检查点失败"), err.message),
  });
  const fork = useMutation({
    mutationFn: () => verbs.fork(sb.id),
    onSuccess: (child) => {
      nav(`/sandboxes/${child.id}`);
      toast.action("success", `${t("Forked to", "已派生到")} ${child.id.slice(0, 8)}`, {
        label: t("Back to parent", "返回父沙箱"),
        onClick: () => nav(`/sandboxes/${sb.id}`),
      });
    },
    onError: (err) => toast.error(t("Fork failed", "派生失败"), err.message),
  });
  const running = sb.state === "RUNNING";

  return (
    <Card title={t("Time travel", "时间旅行")}>
      <div className="space-y-3">
        <form
          className="flex gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (cmdline.trim() && running) exec.mutate();
          }}
        >
          <input
            className={`${inputCls} font-mono`}
            value={cmdline}
            onChange={(e) => setCmdline(e.target.value)}
            placeholder="python3 train.py --step 7"
            aria-label={t("Command to run", "要运行的命令")}
          />
          <Button kind="primary" type="submit" busy={exec.isPending} disabled={!running}>
            {t("Run", "运行")}
          </Button>
        </form>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <Toggle
            checked={withCheckpoint}
            onChange={setWithCheckpoint}
            label={
              <span className="text-[13px]">
                {t("Checkpoint before running", "运行前先检查点")}{" "}
                <span className="text-faint">{t("— the step becomes a rollback anchor", "—— 该步骤成为回滚锚点")}</span>
              </span>
            }
          />
          <div className="flex items-center gap-2">
            <input
              className={`${inputCls} w-44`}
              value={tag}
              onChange={(e) => setTag(e.target.value)}
              placeholder={t("checkpoint tag", "检查点标签")}
              aria-label={t("Checkpoint tag", "检查点标签")}
            />
            <Button size="sm" onClick={() => checkpoint.mutate()} busy={checkpoint.isPending} disabled={!running}>
              {t("Checkpoint", "检查点")}
            </Button>
            <Tip content={t("Checkpoint now, then branch a new sandbox", "立即检查点，再派生一个新沙箱")}>
              <Button size="sm" onClick={() => fork.mutate()} busy={fork.isPending} disabled={!running}>
                <IconBranch size={12} /> {t("Fork now", "立即派生")}
              </Button>
            </Tip>
          </div>
        </div>
        {!running && <p className="text-xs text-faint">{t("time travel needs RUNNING", "时间旅行需要 RUNNING")}</p>}
        <ErrorNote error={exec.error ?? checkpoint.error ?? fork.error} />
        {result && (
          <div className="rounded-md border border-hairline bg-bg p-3">
            <div className="mb-2 font-mono text-[11px] text-muted tabular-nums">
              {t("exit", "退出码")}{" "}
              <span className={result.exit_code === 0 ? "text-ok" : "text-danger"}>
                {result.exit_code}
              </span>
              {" · "}
              {result.duration_ms}ms
              {result.checkpoint && (
                <span className="text-accent"> · {t("checkpoint", "检查点")} {result.checkpoint}</span>
              )}
              {result.timed_out && <span className="text-danger"> · {t("timed out", "已超时")}</span>}
              {result.truncated && <span className="text-transit"> · {t("truncated", "已截断")}</span>}
            </div>
            <pre className="max-h-56 overflow-auto whitespace-pre-wrap font-mono text-xs text-ink">
              {decodeBytes(result.stdout) || <span className="text-faint">{t("(no stdout)", "（无标准输出）")}</span>}
            </pre>
            {result.stderr && (
              <pre className="mt-2 max-h-40 overflow-auto whitespace-pre-wrap font-mono text-xs text-[#f3a6a2]">
                {decodeBytes(result.stderr)}
              </pre>
            )}
          </div>
        )}
      </div>
    </Card>
  );
}

/* ── Merged timeline ─────────────────────────────────────────────────── */

type Item =
  | { kind: "checkpoint"; at: string; cp: Checkpoint }
  | { kind: "event"; at: string; ev: SandboxEvent };

function Timeline(props: { sb: Sandbox }) {
  const { sb } = props;
  const { t } = useI18n();
  const nav = useNavigate();
  const qc = useQueryClient();
  const checkpoints = useCheckpoints(sb.id);
  const events = useSandboxEvents(sb.id);
  const [rollbackTarget, setRollbackTarget] = useState<string | null>(null);

  const fork = useMutation({
    mutationFn: (cp: string) => verbs.fork(sb.id, cp),
    onSuccess: (child) => {
      nav(`/sandboxes/${child.id}`);
      toast.action("success", `${t("Forked to", "已派生到")} ${child.id.slice(0, 8)}`, {
        label: t("Back to parent", "返回父沙箱"),
        onClick: () => nav(`/sandboxes/${sb.id}`),
      });
    },
    onError: (err) => toast.error(t("Fork failed", "派生失败"), err.message),
  });
  const rollback = useMutation({
    mutationFn: (cp: string) => verbs.rollback(sb.id, cp),
    onSuccess: (_out, cp) => {
      void qc.invalidateQueries({ queryKey: ["sandboxes"] });
      void qc.invalidateQueries({ queryKey: ["sandboxes", sb.id, "checkpoints"] });
      void qc.invalidateQueries({ queryKey: ["sandboxes", sb.id, "events"] });
      toast.success(`${t("Rolled back to", "已回滚到")} ${cp}`);
    },
    onError: (err) => toast.error(t("Rollback failed", "回滚失败"), err.message),
  });

  const loading = checkpoints.isLoading || events.isLoading;
  const items: Item[] = [
    ...(checkpoints.data ?? []).map((cp): Item => ({ kind: "checkpoint", at: cp.created_at, cp })),
    ...(events.data?.events ?? []).map((ev): Item => ({ kind: "event", at: ev.at, ev })),
  ].sort((a, b) => new Date(b.at).getTime() - new Date(a.at).getTime());

  return (
    <Card title={t("Timeline", "时间线")} pad={false}>
      {loading && (
        <div className="space-y-2 p-4">
          <Skeleton className="h-5 w-64" />
          <Skeleton className="h-5 w-52" />
          <Skeleton className="h-5 w-60" />
        </div>
      )}
      {!loading && items.length === 0 && (
        <Empty>{t("No history yet — every state change and checkpoint lands here.", "暂无历史 —— 每次状态变化与检查点都会记录在此。")}</Empty>
      )}
      {items.length > 0 && (
        <ol className="relative">
          {items.map((it, i) => (
            <li
              key={it.kind === "checkpoint" ? `cp-${it.cp.tag}` : `ev-${it.ev.id}`}
              className={`relative flex items-center gap-3 py-2.5 pl-4 pr-4 ${
                i < items.length - 1 ? "border-b border-hairline/60" : ""
              }`}
            >
              {it.kind === "checkpoint" ? (
                <CheckpointRow
                  cp={it.cp}
                  onFork={() => fork.mutate(it.cp.tag)}
                  onRollback={() => setRollbackTarget(it.cp.tag)}
                  forkBusy={fork.isPending}
                />
              ) : (
                <EventRow ev={it.ev} />
              )}
            </li>
          ))}
        </ol>
      )}
      <ConfirmDialog
        open={rollbackTarget !== null}
        title={t("Roll back sandbox", "回滚沙箱")}
        body={
          <>
            {t("Rewind to", "回退到")} <Mono className="text-ink">{rollbackTarget}</Mono>
            {t("? Everything after that checkpoint is discarded. Checkpoints with live forks are protected server-side.", "？该检查点之后的一切都将被丢弃。存在活跃派生的检查点在服务端受保护。")}
          </>
        }
        confirmLabel={t("Roll back", "回滚")}
        busy={rollback.isPending}
        onConfirm={() => {
          if (rollbackTarget) rollback.mutate(rollbackTarget);
          setRollbackTarget(null);
        }}
        onClose={() => setRollbackTarget(null)}
      />
    </Card>
  );
}

function CheckpointRow(props: {
  cp: Checkpoint;
  onFork: () => void;
  onRollback: () => void;
  forkBusy: boolean;
}) {
  const { cp } = props;
  const { t } = useI18n();
  return (
    <>
      <span aria-hidden className="size-2 shrink-0 rotate-45 bg-accent" />
      <div className="min-w-0 flex-1">
        <Mono className="text-ink">{cp.tag}</Mono>
        <span className="ml-2 font-mono text-[11px] text-faint">
          {t("checkpoint", "检查点")} · {cp.layer} · {t("seq", "序号")} {cp.seq} · {fmtAge(cp.created_at)} {t("ago", "前")}
        </span>
      </div>
      <div className="flex shrink-0 gap-2">
        <Button size="sm" onClick={props.onFork} busy={props.forkBusy}>
          <IconBranch size={12} /> {t("Fork", "派生")}
        </Button>
        <Button size="sm" kind="danger" onClick={props.onRollback}>
          <IconUndo size={12} /> {t("Roll back", "回滚")}
        </Button>
      </div>
    </>
  );
}

function EventRow(props: { ev: SandboxEvent }) {
  const { ev } = props;
  const { t } = useI18n();
  const meta = STATE_META[ev.to_state as SandboxState];
  const color = meta?.color ?? "var(--color-idle)";
  const errDetail = ev.detail && typeof ev.detail.error === "string" ? ev.detail.error : null;
  return (
    <>
      <span aria-hidden className="size-2 shrink-0 rounded-full" style={{ background: color }} />
      <div className="min-w-0 flex-1">
        <span className="text-[13px] text-ink">{stateLabel(ev.to_state as SandboxState, t)}</span>
        {ev.from_state && (
          <span className="ml-2 font-mono text-[11px] text-faint">{t("from", "自")} {ev.from_state}</span>
        )}
        {errDetail && (
          <span className="ml-2 rounded bg-danger/10 px-1.5 py-0.5 font-mono text-[11px] text-danger">
            {errDetail}
          </span>
        )}
      </div>
      <Tip mono content={new Date(ev.at).toLocaleString()}>
        <span className="shrink-0 cursor-default font-mono text-[11px] text-faint">
          {fmtAge(ev.at)} {t("ago", "前")}
        </span>
      </Tip>
    </>
  );
}
