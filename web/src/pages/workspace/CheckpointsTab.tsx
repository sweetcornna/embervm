// Checkpoints tab: create/tag checkpoints, fork, rollback. (The merged
// state-transition timeline arrives with the /events endpoint.)

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { fmtAge } from "../../api/client";
import { useCheckpoints, verbs } from "../../api/hooks";
import type { Sandbox } from "../../api/types";
import { IconBranch, IconUndo } from "../../components/icons";
import {
  Button,
  Card,
  ConfirmDialog,
  Empty,
  ErrorNote,
  Field,
  Mono,
  Skeleton,
  inputCls,
} from "../../components/ui";
import { toast } from "../../lib/toast";

export function CheckpointsTab(props: { sb: Sandbox }) {
  const { sb } = props;
  const nav = useNavigate();
  const qc = useQueryClient();
  const { data, isLoading } = useCheckpoints(sb.id);
  const [tag, setTag] = useState("");
  const [rollbackTarget, setRollbackTarget] = useState<string | null>(null);

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ["sandboxes"] });
    void qc.invalidateQueries({ queryKey: ["sandboxes", sb.id, "checkpoints"] });
  };
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
    mutationFn: (cp?: string) => verbs.fork(sb.id, cp),
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
      invalidate();
      toast.success(`Rolled back to ${cp}`);
    },
    onError: (err) => toast.error("Rollback failed", err.message),
  });

  return (
    <div className="mx-auto max-w-4xl space-y-4 p-4">
      <Card title="New checkpoint">
        <div className="flex flex-wrap items-end gap-2">
          <div className="grow">
            <Field label="Tag" hint="Blank = auto-named cp<seq>. Every checkpoint is a fork/rollback anchor.">
              <input
                className={inputCls}
                value={tag}
                onChange={(e) => setTag(e.target.value)}
                placeholder="before-experiment"
              />
            </Field>
          </div>
          <Button
            onClick={() => checkpoint.mutate()}
            busy={checkpoint.isPending}
            disabled={sb.state !== "RUNNING"}
          >
            Checkpoint
          </Button>
          <Button
            onClick={() => fork.mutate(undefined)}
            busy={fork.isPending}
            title="Checkpoint now, then branch"
          >
            <IconBranch size={13} /> Fork now
          </Button>
        </div>
        {sb.state !== "RUNNING" && (
          <p className="mt-2 text-xs text-faint">checkpointing needs RUNNING</p>
        )}
        <ErrorNote error={checkpoint.error ?? fork.error ?? rollback.error} />
      </Card>

      <Card title="Timeline" pad={false}>
        {isLoading && (
          <div className="space-y-2 p-4">
            <Skeleton className="h-5 w-64" />
            <Skeleton className="h-5 w-52" />
          </div>
        )}
        {data && data.length === 0 && (
          <Empty>No checkpoints yet — take one above, or Snapshot from the header.</Empty>
        )}
        {data && data.length > 0 && (
          <ul className="divide-y divide-hairline">
            {[...data].reverse().map((cp) => (
              <li key={cp.tag} className="flex items-center justify-between gap-3 px-4 py-2.5">
                <div className="flex min-w-0 items-center gap-2.5">
                  <span aria-hidden className="size-2 shrink-0 rotate-45 bg-accent" />
                  <div className="min-w-0">
                    <Mono className="text-ink">{cp.tag}</Mono>
                    <span className="ml-2 font-mono text-[11px] text-faint">
                      {cp.layer} · seq {cp.seq} · {fmtAge(cp.created_at)} ago
                    </span>
                  </div>
                </div>
                <div className="flex shrink-0 gap-2">
                  <Button size="sm" onClick={() => fork.mutate(cp.tag)} busy={fork.isPending}>
                    <IconBranch size={12} /> Fork
                  </Button>
                  <Button size="sm" kind="danger" onClick={() => setRollbackTarget(cp.tag)}>
                    <IconUndo size={12} /> Roll back
                  </Button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </Card>

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
    </div>
  );
}
