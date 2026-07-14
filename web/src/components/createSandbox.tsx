// The one "New sandbox" dialog — shared by the Sandboxes page, the Overview
// empty state, and (later) the command palette. Creates and navigates into
// the new workspace.
//
// M7: elastic is the default. Simple mode sends NO geometry (the server's
// default-elastic resolution is the single truth — the toast reads the
// response back) or just a ceiling preset; Advanced exposes explicit
// base/max/autoscale; Fixed keeps the pre-M7 explicit-base shape.

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { fmtMiB } from "../api/client";
import { useTemplates, verbs } from "../api/hooks";
import type { CreateSandboxRequest } from "../api/types";
import { useI18n } from "../lib/i18n";
import {
  DEFAULT_BASE,
  DEFAULT_CEILING,
  ELASTIC_PRESETS,
  slotRoundingHint,
  validateGeometry,
} from "../lib/geometry";
import { toast } from "../lib/toast";
import { Button, Dialog, ErrorNote, Field, Toggle, inputCls } from "./ui";

type Mode = "elastic" | "fixed";

export function CreateSandboxDialog(props: { open: boolean; onClose: () => void }) {
  const { t } = useI18n();
  const templates = useTemplates();
  const qc = useQueryClient();
  const nav = useNavigate();
  const ready = (templates.data ?? []).filter((tpl) => tpl.state === "READY");

  const [mode, setMode] = useState<Mode>("elastic");
  const [advanced, setAdvanced] = useState(false);
  const [form, setForm] = useState({
    template_id: "",
    preset: "standard",
    // Advanced-elastic / fixed fields.
    vcpus: DEFAULT_BASE.vcpus,
    memory_mib: DEFAULT_BASE.memory_mib,
    data_disk_gib: 15,
    max_memory_mib: DEFAULT_CEILING.memory_mib,
    max_vcpus: DEFAULT_CEILING.vcpus,
    autoscale: true,
    egress: "nat" as "nat" | "none",
  });
  const set = <K extends keyof typeof form>(k: K, v: (typeof form)[K]) =>
    setForm((f) => ({ ...f, [k]: v }));

  const create = useMutation({
    mutationFn: (body: CreateSandboxRequest) => verbs.createSandbox(body),
    onSuccess: (sb) => {
      void qc.invalidateQueries({ queryKey: ["sandboxes"] });
      // Report what the SERVER resolved, not what the form assumed.
      const elastic = (sb.max_memory_mib ?? 0) > 0 || (sb.max_vcpus ?? 0) > 0;
      toast.success(
        elastic
          ? t(
              `Sandbox created — ${fmtMiB(sb.memory_mib)} → up to ${fmtMiB(sb.max_memory_mib || sb.memory_mib)}` +
                (sb.autoscale ? " · autoscale on" : ""),
              `沙箱已创建 —— ${fmtMiB(sb.memory_mib)} → 上限 ${fmtMiB(sb.max_memory_mib || sb.memory_mib)}` +
                (sb.autoscale ? " · 自动伸缩已开启" : ""),
            )
          : t(
              `Sandbox created — fixed ${fmtMiB(sb.memory_mib)} / ${sb.vcpus} vCPU`,
              `沙箱已创建 —— 固定 ${fmtMiB(sb.memory_mib)} / ${sb.vcpus} vCPU`,
            ),
      );
      props.onClose();
      nav(`/sandboxes/${sb.id}`);
    },
  });

  const validationError =
    mode === "fixed"
      ? validateGeometry(
          { vcpus: form.vcpus, memory_mib: form.memory_mib, data_disk_gib: form.data_disk_gib },
          t,
        )
      : advanced
        ? validateGeometry(
            {
              vcpus: form.vcpus,
              memory_mib: form.memory_mib,
              data_disk_gib: form.data_disk_gib,
              max_vcpus: form.max_vcpus,
              max_memory_mib: form.max_memory_mib,
              autoscale: form.autoscale,
            },
            t,
          )
        : null;

  function submit(e: React.FormEvent) {
    e.preventDefault();
    if (validationError) return;
    const body: CreateSandboxRequest = {
      template_id: form.template_id || ready[0]?.id || "",
      egress: form.egress,
    };
    if (form.data_disk_gib !== 15) body.data_disk_gib = form.data_disk_gib;
    if (mode === "fixed") {
      body.vcpus = form.vcpus;
      body.memory_mib = form.memory_mib;
    } else if (advanced) {
      body.vcpus = form.vcpus;
      body.memory_mib = form.memory_mib;
      body.max_memory_mib = form.max_memory_mib;
      body.max_vcpus = form.max_vcpus;
      body.autoscale = form.autoscale;
    } else {
      // Simple elastic: the server's defaults are the contract. Only a
      // non-default preset adds ceilings.
      const preset = ELASTIC_PRESETS.find((p) => p.key === form.preset);
      if (preset?.max_memory_mib) body.max_memory_mib = preset.max_memory_mib;
      if (preset?.max_vcpus) body.max_vcpus = preset.max_vcpus;
    }
    create.mutate(body);
  }

  const segBtn = (active: boolean) =>
    `flex-1 rounded px-3 py-1.5 text-sm font-medium transition-colors ${
      active ? "bg-raised text-ink shadow-[var(--shadow-raised)]" : "text-muted hover:text-ink"
    }`;

  return (
    <Dialog title={t("New sandbox", "新建沙箱")} open={props.open} onClose={props.onClose}>
      <form onSubmit={submit} className="space-y-4">
        <Field label={t("Template", "模板")}>
          <select
            className={inputCls}
            value={form.template_id || ready[0]?.id || ""}
            onChange={(e) => set("template_id", e.target.value)}
          >
            {ready.length === 0 && <option value="">{t("No READY templates — build one first", "暂无 READY 模板 —— 请先构建一个")}</option>}
            {ready.map((tpl) => (
              <option key={tpl.id} value={tpl.id}>
                {tpl.name} ({tpl.image})
              </option>
            ))}
          </select>
        </Field>

        <div className="flex gap-1 rounded-md border border-hairline bg-bg p-1" role="tablist">
          <button type="button" role="tab" aria-selected={mode === "elastic"} className={segBtn(mode === "elastic")} onClick={() => setMode("elastic")}>
            {t("Elastic (recommended)", "弹性（推荐）")}
          </button>
          <button type="button" role="tab" aria-selected={mode === "fixed"} className={segBtn(mode === "fixed")} onClick={() => setMode("fixed")}>
            {t("Fixed", "固定")}
          </button>
        </div>

        {mode === "elastic" && (
          <div className="space-y-3 rounded-md border border-hairline bg-bg p-3">
            <p className="text-xs text-muted">
              {t(
                `Starts small (${fmtMiB(DEFAULT_BASE.memory_mib)} / ${DEFAULT_BASE.vcpus} vCPU) and grows under pressure up to the ceiling.`,
                `从小规格（${fmtMiB(DEFAULT_BASE.memory_mib)} / ${DEFAULT_BASE.vcpus} vCPU）启动，压力下自动增长至上限。`,
              )}
            </p>
            {!advanced && (
              <div className="flex flex-wrap gap-2" role="radiogroup" aria-label={t("Ceiling preset", "上限预设")}>
                {ELASTIC_PRESETS.map((p) => {
                  const active = form.preset === p.key;
                  const maxMem = p.max_memory_mib ?? DEFAULT_CEILING.memory_mib;
                  const maxCpu = p.max_vcpus ?? DEFAULT_CEILING.vcpus;
                  return (
                    <button
                      key={p.key}
                      type="button"
                      role="radio"
                      aria-checked={active}
                      onClick={() => set("preset", p.key)}
                      className={`rounded-md border px-3 py-1.5 text-left text-xs transition-colors ${
                        active ? "border-accent bg-accent/10 text-ink" : "border-hairline text-muted hover:border-border hover:text-ink"
                      }`}
                    >
                      <span className="block font-medium">{t(p.label, p.zh)}</span>
                      <span className="block font-mono tabular-nums text-[11px]">
                        {t("up to", "上限")} {fmtMiB(maxMem)} · {maxCpu} vCPU
                      </span>
                    </button>
                  );
                })}
              </div>
            )}
            <button
              type="button"
              className="text-xs text-accent hover:underline"
              onClick={() => setAdvanced((v) => !v)}
            >
              {advanced ? t("Hide advanced", "收起高级选项") : t("Advanced — explicit base & ceiling", "高级 —— 显式基础与上限")}
            </button>
            {advanced && (
              <div className="grid grid-cols-2 gap-3 border-t border-hairline pt-3">
                <Field label={t("Base vCPUs", "基础 vCPU")}>
                  <input className={inputCls} type="number" min={1} max={64} value={form.vcpus} onChange={(e) => set("vcpus", Number(e.target.value))} />
                </Field>
                <Field label={t("Base memory MiB", "基础内存 MiB")}>
                  <input className={inputCls} type="number" min={128} step={128} value={form.memory_mib} onChange={(e) => set("memory_mib", Number(e.target.value))} />
                </Field>
                <Field label={t("Max vCPUs", "最大 vCPU")}>
                  <input className={inputCls} type="number" min={form.vcpus} max={64} value={form.max_vcpus} onChange={(e) => set("max_vcpus", Number(e.target.value))} />
                </Field>
                <Field
                  label={t("Max memory MiB", "最大内存 MiB")}
                  hint={slotRoundingHint(form.memory_mib, form.max_memory_mib, t) ?? t("Rounded up to 128 MiB slots.", "向上取整到 128 MiB 的槽位。")}
                >
                  <input className={inputCls} type="number" min={form.memory_mib} step={128} value={form.max_memory_mib} onChange={(e) => set("max_memory_mib", Number(e.target.value))} />
                </Field>
                <div className="col-span-2">
                  <Toggle
                    checked={form.autoscale}
                    onChange={(v) => set("autoscale", v)}
                    label={t("Autoscale on guest pressure", "按 guest 压力自动伸缩")}
                  />
                </div>
              </div>
            )}
          </div>
        )}

        {mode === "fixed" && (
          <div className="space-y-3 rounded-md border border-hairline bg-bg p-3">
            <p className="text-xs text-muted">
              {t(
                "Fixed geometry cannot be resized or autoscaled later.",
                "固定规格创建后不可再调整、不可自动伸缩。",
              )}
            </p>
            <div className="grid grid-cols-2 gap-3">
              <Field label={t("vCPUs", "vCPU 数")}>
                <input className={inputCls} type="number" min={1} max={64} value={form.vcpus} onChange={(e) => set("vcpus", Number(e.target.value))} />
              </Field>
              <Field label={t("Memory MiB", "内存 MiB")}>
                <input className={inputCls} type="number" min={128} step={128} value={form.memory_mib} onChange={(e) => set("memory_mib", Number(e.target.value))} />
              </Field>
            </div>
          </div>
        )}

        <div className="grid grid-cols-2 gap-3">
          <Field label={t("Disk GiB", "磁盘 GiB")}>
            <input className={inputCls} type="number" min={1} max={4096} value={form.data_disk_gib} onChange={(e) => set("data_disk_gib", Number(e.target.value))} />
          </Field>
          <Field label={t("Egress", "出网")}>
            <select className={inputCls} value={form.egress} onChange={(e) => set("egress", e.target.value as "nat" | "none")}>
              <option value="nat">{t("nat — outbound internet", "nat — 可出公网")}</option>
              <option value="none">{t("none — no outbound network", "none — 无出网")}</option>
            </select>
          </Field>
        </div>

        {validationError && <p className="text-xs text-danger">{validationError}</p>}
        <ErrorNote error={create.error} />
        <div className="flex justify-end gap-2">
          <Button onClick={props.onClose}>{t("Cancel", "取消")}</Button>
          <Button
            kind="primary"
            type="submit"
            busy={create.isPending}
            disabled={ready.length === 0 || validationError !== null}
          >
            {t("Create sandbox", "创建沙箱")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
