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
import { useI18n } from "../../lib/i18n";
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
  const { t } = useI18n();
  const { data: nodes } = useNodes();
  const [target, setTarget] = useState(""); // "" = scheduler bin-packs
  const migrate = useSandboxAction(() => verbs.migrate(sb.id, target || undefined), {
    sandboxId: sb.id,
    onSuccess: (moved) => toast.success(t("Migrated", "已迁移"), `${t("now on node", "现位于节点")} ${moved.node_id ?? "?"}`),
    onError: toastError(t("Migrate failed", "迁移失败")),
  });
  const movable = sb.state === "RUNNING" || sb.state === "PAUSED_HOT";
  const candidates = (nodes ?? []).filter((n) => n.state === "up" && n.id !== sb.node_id);

  return (
    <Card title={t("Placement", "部署位置")}>
      <div className="space-y-3">
        <p className="text-[13px] text-muted">
          {t("Currently on", "当前位于")} <Mono className="text-ink">{sb.node_id || "—"}</Mono>
          {t(". Migration moves the live VM — snapshot, cross-node restore, resume; open terminals reconnect after the move.", "。迁移会移动运行中的 VM —— 快照、跨节点恢复、恢复运行；打开的终端会在迁移后重新连接。")}
        </p>
        <div className="flex flex-wrap items-end gap-2">
          <div className="w-72">
            <Field label={t("Target node", "目标节点")}>
              <select
                className="w-full rounded-md border border-border bg-bg px-2.5 py-1.5 text-[13px] text-ink focus:border-accent focus:outline-none"
                value={target}
                onChange={(e) => setTarget(e.target.value)}
                aria-label={t("Target node", "目标节点")}
              >
                <option value="">{t("let the scheduler pick (bin-pack)", "由调度器选择（装箱）")}</option>
                {candidates.map((n) => (
                  <option key={n.id} value={n.id}>
                    {n.id} —{" "}
                    {(n.mem_budget_mib ?? 0) > 0
                      ? `${fmtMiB(Math.max(0, (n.mem_budget_mib ?? 0) - n.used_mib))} ${t("free of budget", "预算余量")}`
                      : `${fmtMiB(n.used_mib)}${n.capacity_mib > 0 ? ` / ${fmtMiB(n.capacity_mib)}` : ""} ${t("used", "已用")}`}{" "}
                    · {n.active_sandboxes} {t("active", "活跃")}
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
            <IconNode size={13} /> {t("Migrate", "迁移")}
          </Button>
        </div>
        {!movable && (
          <p className="text-xs text-faint">{t("migration needs RUNNING or PAUSED_HOT", "迁移需要 RUNNING 或 PAUSED_HOT")}</p>
        )}
        <ErrorNote error={migrate.error} />
      </div>
    </Card>
  );
}

function RestoreCard(props: { sb: Sandbox }) {
  const { sb } = props;
  const { t } = useI18n();
  const nav = useNavigate();
  const restore = useSandboxAction(() => verbs.restoreArtifacts(sb.id), {
    onSuccess: (out) => {
      toast.success(t("Artifacts restored", "产物已恢复"), `${t("new sandbox", "新沙箱")} ${out.sandbox.id.slice(0, 8)}`);
      nav(`/sandboxes/${out.sandbox.id}`);
    },
    onError: toastError(t("Restore failed", "恢复失败")),
  });
  return (
    <Card title={t("Recycled — restore artifacts", "已回收 —— 恢复产物")}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="max-w-lg text-[13px] text-muted">
          {t("This sandbox was recycled: only its extracted artifacts remain in the cold store. Restore seeds a", "此沙箱已被回收：仅其提取的产物保留在冷存储中。恢复会以这些文件创建一个")}{" "}
          <em>{t("new", "新")}</em>{" "}
          {t("sandbox with those files.", "沙箱。")}
        </p>
        <Button kind="primary" onClick={() => restore.mutate()} busy={restore.isPending}>
          <IconArrowRight size={13} /> {t("Restore into new sandbox", "恢复到新沙箱")}
        </Button>
      </div>
      <ErrorNote error={restore.error} />
    </Card>
  );
}

function GeometryCard(props: { sb: Sandbox }) {
  const { sb } = props;
  const { t } = useI18n();
  const rows: Array<[string, string]> = [
    [t("memory", "内存"), fmtMiB(sb.memory_mib)],
    [t("base memory", "基础内存"), sb.base_memory_mib ? fmtMiB(sb.base_memory_mib) : fmtMiB(sb.memory_mib)],
    [t("memory ceiling", "内存上限"), sb.max_memory_mib ? fmtMiB(sb.max_memory_mib) : t("fixed", "固定")],
    [t("vcpus", "vCPU"), String(sb.vcpus)],
    [t("base vcpus", "基础 vCPU"), String(sb.base_vcpus || sb.vcpus)],
    [t("vcpu ceiling", "vCPU 上限"), sb.max_vcpus ? String(sb.max_vcpus) : t("fixed", "固定")],
    [t("data disk", "数据盘"), `${sb.data_disk_gib} GiB`],
    [t("autoscale", "自动伸缩"), sb.autoscale ? t("on (guest pressure)", "开启（按 guest 压力）") : t("off", "关闭")],
  ];
  return (
    <Card title={t("Geometry", "规格")}>
      <dl className="grid grid-cols-2 gap-x-4 gap-y-2.5 sm:grid-cols-3">
        {rows.map(([k, v]) => (
          <div key={k}>
            <dt className="font-mono text-[10px] uppercase tracking-[0.12em] text-faint">{k}</dt>
            <dd className="mt-0.5 font-mono text-[13px] text-ink">{v}</dd>
          </div>
        ))}
      </dl>
      <p className="mt-3 text-xs text-faint">
        {t("Ceilings are fixed at create; live values move on the Overview tab's resize panel.", "上限在创建时固定；实时值可在「总览」标签的调整面板中修改。")}
      </p>
    </Card>
  );
}

function DangerCard(props: { sb: Sandbox }) {
  const { sb } = props;
  const { t } = useI18n();
  const nav = useNavigate();
  const confirm = useConfirm();
  const kill = useSandboxAction(() => verbs.kill(sb.id), {
    onSuccess: () => {
      disposeTermSandbox(sb.id);
      toast.success(`${t("Sandbox", "沙箱")} ${sb.id.slice(0, 8)} ${t("destroyed", "已销毁")}`);
      nav("/sandboxes");
    },
    onError: toastError(t("Kill failed", "销毁失败")),
  });
  return (
    <Card title={t("Danger zone", "危险操作")}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="max-w-lg text-[13px] text-muted">
          {t("Killing destroys the VM, its disk, checkpoints and snapshots. Sandboxes with live forks are protected server-side.", "销毁将删除 VM 及其磁盘、检查点和快照。存在活跃派生的沙箱在服务端受保护。")}
        </p>
        <Button kind="danger" onClick={() => confirm.ask(() => kill.mutate())} busy={kill.isPending}>
          {t("Kill sandbox…", "销毁沙箱…")}
        </Button>
      </div>
      <ConfirmDialog
        open={confirm.open}
        title={t("Kill sandbox", "销毁沙箱")}
        body={
          <>
            {t("Destroy", "永久销毁")} <Mono className="text-ink">{sb.id.slice(0, 8)}</Mono>
            {t(" permanently? This cannot be undone.", "？此操作不可撤销。")}
          </>
        }
        confirmLabel={t("Kill sandbox", "销毁沙箱")}
        busy={kill.isPending}
        onConfirm={confirm.confirm}
        onClose={confirm.close}
      />
    </Card>
  );
}
