// Settings tab: placement (migrate with an explicit node picker), the
// RECYCLED restore-artifacts flow, geometry readout, and the danger zone.

import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { fmtMiB } from "../../api/client";
import { useNodes, useSandboxAction, verbs } from "../../api/hooks";
import type { Sandbox } from "../../api/types";
import { IconArrowRight, IconNode } from "../../components/icons";
import {
  Button,
  Card,
  ConfirmDialog,
  ErrorNote,
  Field,
  Mono,
  useConfirm,
} from "../../components/ui";
import { disposeTermSandbox } from "../../lib/termBridge";
import { toast, toastError } from "../../lib/toast";

export function SettingsTab(props: { sb: Sandbox }) {
  const { sb } = props;
  return (
    <div className="mx-auto max-w-4xl space-y-4 p-4">
      <PlacementCard sb={sb} />
      {sb.state === "RECYCLED" && <RestoreCard sb={sb} />}
      <GeometryCard sb={sb} />
      <DangerCard sb={sb} />
    </div>
  );
}

function PlacementCard(props: { sb: Sandbox }) {
  const { sb } = props;
  const { data: nodes } = useNodes();
  const [target, setTarget] = useState(""); // "" = scheduler bin-packs
  const migrate = useSandboxAction(() => verbs.migrate(sb.id, target || undefined), {
    sandboxId: sb.id,
    onSuccess: (moved) => toast.success("Migrated", `now on node ${moved.node_id ?? "?"}`),
    onError: toastError("Migrate failed"),
  });
  const movable = sb.state === "RUNNING" || sb.state === "PAUSED_HOT";
  const candidates = (nodes ?? []).filter((n) => n.state === "up" && n.id !== sb.node_id);

  return (
    <Card title="Placement">
      <div className="space-y-3">
        <p className="text-[13px] text-muted">
          Currently on <Mono className="text-ink">{sb.node_id || "—"}</Mono>. Migration moves the
          live VM — snapshot, cross-node restore, resume; open terminals reconnect after the move.
        </p>
        <div className="flex flex-wrap items-end gap-2">
          <div className="w-72">
            <Field label="Target node">
              <select
                className="w-full rounded-md border border-border bg-bg px-2.5 py-1.5 text-[13px] text-ink focus:border-accent focus:outline-none"
                value={target}
                onChange={(e) => setTarget(e.target.value)}
                aria-label="Target node"
              >
                <option value="">let the scheduler pick (bin-pack)</option>
                {candidates.map((n) => (
                  <option key={n.id} value={n.id}>
                    {n.id} — {fmtMiB(n.used_mib)}
                    {n.capacity_mib > 0 ? ` / ${fmtMiB(n.capacity_mib)}` : ""} used ·{" "}
                    {n.active_sandboxes} active
                  </option>
                ))}
              </select>
            </Field>
          </div>
          <Button
            onClick={() => migrate.mutate()}
            busy={migrate.isPending}
            disabled={!movable || (candidates.length === 0 && target === "" && (nodes ?? []).length <= 1)}
          >
            <IconNode size={13} /> Migrate
          </Button>
        </div>
        {!movable && (
          <p className="text-xs text-faint">migration needs RUNNING or PAUSED_HOT</p>
        )}
        <ErrorNote error={migrate.error} />
      </div>
    </Card>
  );
}

function RestoreCard(props: { sb: Sandbox }) {
  const { sb } = props;
  const nav = useNavigate();
  const restore = useSandboxAction(() => verbs.restoreArtifacts(sb.id), {
    onSuccess: (out) => {
      toast.success("Artifacts restored", `new sandbox ${out.sandbox.id.slice(0, 8)}`);
      nav(`/sandboxes/${out.sandbox.id}`);
    },
    onError: toastError("Restore failed"),
  });
  return (
    <Card title="Recycled — restore artifacts">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="max-w-lg text-[13px] text-muted">
          This sandbox was recycled: only its extracted artifacts remain in the cold store. Restore
          seeds a <em>new</em> sandbox with those files.
        </p>
        <Button kind="primary" onClick={() => restore.mutate()} busy={restore.isPending}>
          <IconArrowRight size={13} /> Restore into new sandbox
        </Button>
      </div>
      <ErrorNote error={restore.error} />
    </Card>
  );
}

function GeometryCard(props: { sb: Sandbox }) {
  const { sb } = props;
  const rows: Array<[string, string]> = [
    ["memory", fmtMiB(sb.memory_mib)],
    ["memory ceiling", sb.max_memory_mib ? fmtMiB(sb.max_memory_mib) : "fixed"],
    ["vcpus", String(sb.vcpus)],
    ["vcpu ceiling", sb.max_vcpus ? String(sb.max_vcpus) : "fixed"],
    ["data disk", `${sb.data_disk_gib} GiB`],
    ["autoscale", sb.autoscale ? "on (guest pressure)" : "off"],
  ];
  return (
    <Card title="Geometry">
      <dl className="grid grid-cols-2 gap-x-4 gap-y-2.5 sm:grid-cols-3">
        {rows.map(([k, v]) => (
          <div key={k}>
            <dt className="font-mono text-[10px] uppercase tracking-[0.12em] text-faint">{k}</dt>
            <dd className="mt-0.5 font-mono text-[13px] text-ink">{v}</dd>
          </div>
        ))}
      </dl>
      <p className="mt-3 text-xs text-faint">
        Ceilings are fixed at create; live values move on the Overview tab's resize panel.
      </p>
    </Card>
  );
}

function DangerCard(props: { sb: Sandbox }) {
  const { sb } = props;
  const nav = useNavigate();
  const confirm = useConfirm();
  const kill = useSandboxAction(() => verbs.kill(sb.id), {
    onSuccess: () => {
      disposeTermSandbox(sb.id);
      toast.success(`Sandbox ${sb.id.slice(0, 8)} destroyed`);
      nav("/sandboxes");
    },
    onError: toastError("Kill failed"),
  });
  return (
    <Card title="Danger zone">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="max-w-lg text-[13px] text-muted">
          Killing destroys the VM, its disk, checkpoints and snapshots. Sandboxes with live forks
          are protected server-side.
        </p>
        <Button kind="danger" onClick={() => confirm.ask(() => kill.mutate())} busy={kill.isPending}>
          Kill sandbox…
        </Button>
      </div>
      <ConfirmDialog
        open={confirm.open}
        title="Kill sandbox"
        body={
          <>
            Destroy <Mono className="text-ink">{sb.id.slice(0, 8)}</Mono> permanently? This cannot
            be undone.
          </>
        }
        confirmLabel="Kill sandbox"
        busy={kill.isPending}
        onConfirm={confirm.confirm}
        onClose={confirm.close}
      />
    </Card>
  );
}
