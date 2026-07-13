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
import { STATE_META } from "../../components/status";
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
        toast.success(`Step checkpointed as ${out.checkpoint}`, "roll back to undo this step");
      }
    },
    onError: (err) => toast.error("Exec failed", err.message),
  });
  const checkpoint = useMutation({
    mutationFn: () => verbs.checkpoint(sb.id, tag.trim() || undefined),
    onSuccess: (cp) => {
      setTag("");
      invalidate();
      toast.success(`Checkpoint ${cp.tag}`, `layer ${cp.layer}`);
    },
    onError: (err) => toast.error("Checkpoint failed", err.message),
  });
  const fork = useMutation({
    mutationFn: () => verbs.fork(sb.id),
    onSuccess: (child) => {
      nav(`/sandboxes/${child.id}`);
      toast.action("success", `Forked to ${child.id.slice(0, 8)}`, {
        label: "Back to parent",
        onClick: () => nav(`/sandboxes/${sb.id}`),
      });
    },
    onError: (err) => toast.error("Fork failed", err.message),
  });
  const running = sb.state === "RUNNING";

  return (
    <Card title="Time travel">
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
            aria-label="Command to run"
          />
          <Button kind="primary" type="submit" busy={exec.isPending} disabled={!running}>
            Run
          </Button>
        </form>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <Toggle
            checked={withCheckpoint}
            onChange={setWithCheckpoint}
            label={
              <span className="text-[13px]">
                Checkpoint before running{" "}
                <span className="text-faint">— the step becomes a rollback anchor</span>
              </span>
            }
          />
          <div className="flex items-center gap-2">
            <input
              className={`${inputCls} w-44`}
              value={tag}
              onChange={(e) => setTag(e.target.value)}
              placeholder="checkpoint tag"
              aria-label="Checkpoint tag"
            />
            <Button size="sm" onClick={() => checkpoint.mutate()} busy={checkpoint.isPending} disabled={!running}>
              Checkpoint
            </Button>
            <Tip content="Checkpoint now, then branch a new sandbox">
              <Button size="sm" onClick={() => fork.mutate()} busy={fork.isPending} disabled={!running}>
                <IconBranch size={12} /> Fork now
              </Button>
            </Tip>
          </div>
        </div>
        {!running && <p className="text-xs text-faint">time travel needs RUNNING</p>}
        <ErrorNote error={exec.error ?? checkpoint.error ?? fork.error} />
        {result && (
          <div className="rounded-md border border-hairline bg-bg p-3">
            <div className="mb-2 font-mono text-[11px] text-muted tabular-nums">
              exit{" "}
              <span className={result.exit_code === 0 ? "text-ok" : "text-danger"}>
                {result.exit_code}
              </span>
              {" · "}
              {result.duration_ms}ms
              {result.checkpoint && (
                <span className="text-accent"> · checkpoint {result.checkpoint}</span>
              )}
              {result.timed_out && <span className="text-danger"> · timed out</span>}
              {result.truncated && <span className="text-transit"> · truncated</span>}
            </div>
            <pre className="max-h-56 overflow-auto whitespace-pre-wrap font-mono text-xs text-ink">
              {decodeBytes(result.stdout) || <span className="text-faint">(no stdout)</span>}
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
  const nav = useNavigate();
  const qc = useQueryClient();
  const checkpoints = useCheckpoints(sb.id);
  const events = useSandboxEvents(sb.id);
  const [rollbackTarget, setRollbackTarget] = useState<string | null>(null);

  const fork = useMutation({
    mutationFn: (cp: string) => verbs.fork(sb.id, cp),
    onSuccess: (child) => {
      nav(`/sandboxes/${child.id}`);
      toast.action("success", `Forked to ${child.id.slice(0, 8)}`, {
        label: "Back to parent",
        onClick: () => nav(`/sandboxes/${sb.id}`),
      });
    },
    onError: (err) => toast.error("Fork failed", err.message),
  });
  const rollback = useMutation({
    mutationFn: (cp: string) => verbs.rollback(sb.id, cp),
    onSuccess: (_out, cp) => {
      void qc.invalidateQueries({ queryKey: ["sandboxes"] });
      void qc.invalidateQueries({ queryKey: ["sandboxes", sb.id, "checkpoints"] });
      void qc.invalidateQueries({ queryKey: ["sandboxes", sb.id, "events"] });
      toast.success(`Rolled back to ${cp}`);
    },
    onError: (err) => toast.error("Rollback failed", err.message),
  });

  const loading = checkpoints.isLoading || events.isLoading;
  const items: Item[] = [
    ...(checkpoints.data ?? []).map((cp): Item => ({ kind: "checkpoint", at: cp.created_at, cp })),
    ...(events.data?.events ?? []).map((ev): Item => ({ kind: "event", at: ev.at, ev })),
  ].sort((a, b) => new Date(b.at).getTime() - new Date(a.at).getTime());

  return (
    <Card title="Timeline" pad={false}>
      {loading && (
        <div className="space-y-2 p-4">
          <Skeleton className="h-5 w-64" />
          <Skeleton className="h-5 w-52" />
          <Skeleton className="h-5 w-60" />
        </div>
      )}
      {!loading && items.length === 0 && (
        <Empty>No history yet — every state change and checkpoint lands here.</Empty>
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
        title="Roll back sandbox"
        body={
          <>
            Rewind to <Mono className="text-ink">{rollbackTarget}</Mono>? Everything after that
            checkpoint is discarded. Checkpoints with live forks are protected server-side.
          </>
        }
        confirmLabel="Roll back"
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
  return (
    <>
      <span aria-hidden className="size-2 shrink-0 rotate-45 bg-accent" />
      <div className="min-w-0 flex-1">
        <Mono className="text-ink">{cp.tag}</Mono>
        <span className="ml-2 font-mono text-[11px] text-faint">
          checkpoint · {cp.layer} · seq {cp.seq} · {fmtAge(cp.created_at)} ago
        </span>
      </div>
      <div className="flex shrink-0 gap-2">
        <Button size="sm" onClick={props.onFork} busy={props.forkBusy}>
          <IconBranch size={12} /> Fork
        </Button>
        <Button size="sm" kind="danger" onClick={props.onRollback}>
          <IconUndo size={12} /> Roll back
        </Button>
      </div>
    </>
  );
}

function EventRow(props: { ev: SandboxEvent }) {
  const { ev } = props;
  const meta = STATE_META[ev.to_state as SandboxState];
  const color = meta?.color ?? "var(--color-idle)";
  const errDetail = ev.detail && typeof ev.detail.error === "string" ? ev.detail.error : null;
  return (
    <>
      <span aria-hidden className="size-2 shrink-0 rounded-full" style={{ background: color }} />
      <div className="min-w-0 flex-1">
        <span className="text-[13px] text-ink">{meta?.label ?? ev.to_state}</span>
        {ev.from_state && (
          <span className="ml-2 font-mono text-[11px] text-faint">from {ev.from_state}</span>
        )}
        {errDetail && (
          <span className="ml-2 rounded bg-danger/10 px-1.5 py-0.5 font-mono text-[11px] text-danger">
            {errDetail}
          </span>
        )}
      </div>
      <Tip mono content={new Date(ev.at).toLocaleString()}>
        <span className="shrink-0 cursor-default font-mono text-[11px] text-faint">
          {fmtAge(ev.at)} ago
        </span>
      </Tip>
    </>
  );
}
