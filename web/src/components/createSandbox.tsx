// The one "New sandbox" dialog — shared by the Sandboxes page, the Overview
// empty state, and (later) the command palette. Creates and navigates into
// the new workspace.

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useTemplates, verbs } from "../api/hooks";
import type { CreateSandboxRequest } from "../api/types";
import { useI18n } from "../lib/i18n";
import { Button, Dialog, ErrorNote, Field, Toggle, inputCls } from "./ui";

export function CreateSandboxDialog(props: { open: boolean; onClose: () => void }) {
  const { t } = useI18n();
  const templates = useTemplates();
  const qc = useQueryClient();
  const nav = useNavigate();
  const ready = (templates.data ?? []).filter((t) => t.state === "READY");

  const [form, setForm] = useState({
    template_id: "",
    vcpus: 1,
    memory_mib: 256,
    data_disk_gib: 15,
    resizable: false,
    max_memory_mib: 1024,
    max_vcpus: 2,
    autoscale: false,
    egress: "nat" as "nat" | "none",
  });
  const set = <K extends keyof typeof form>(k: K, v: (typeof form)[K]) =>
    setForm((f) => ({ ...f, [k]: v }));

  const create = useMutation({
    mutationFn: (body: CreateSandboxRequest) => verbs.createSandbox(body),
    onSuccess: (sb) => {
      void qc.invalidateQueries({ queryKey: ["sandboxes"] });
      props.onClose();
      nav(`/sandboxes/${sb.id}`);
    },
  });

  function submit(e: React.FormEvent) {
    e.preventDefault();
    const body: CreateSandboxRequest = {
      template_id: form.template_id || ready[0]?.id || "",
      vcpus: form.vcpus,
      memory_mib: form.memory_mib,
      data_disk_gib: form.data_disk_gib,
      egress: form.egress,
    };
    if (form.resizable) {
      body.max_memory_mib = form.max_memory_mib;
      body.max_vcpus = form.max_vcpus;
      body.autoscale = form.autoscale;
    }
    create.mutate(body);
  }

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
        <div className="grid grid-cols-3 gap-3">
          <Field label={t("vCPUs", "vCPU 数")}>
            <input
              className={inputCls}
              type="number"
              min={1}
              max={64}
              value={form.vcpus}
              onChange={(e) => set("vcpus", Number(e.target.value))}
            />
          </Field>
          <Field label={t("Memory MiB", "内存 MiB")}>
            <input
              className={inputCls}
              type="number"
              min={128}
              step={128}
              value={form.memory_mib}
              onChange={(e) => set("memory_mib", Number(e.target.value))}
            />
          </Field>
          <Field label={t("Disk GiB", "磁盘 GiB")}>
            <input
              className={inputCls}
              type="number"
              min={1}
              max={4096}
              value={form.data_disk_gib}
              onChange={(e) => set("data_disk_gib", Number(e.target.value))}
            />
          </Field>
        </div>

        <div className="rounded-md border border-hairline bg-bg p-3">
          <Toggle
            checked={form.resizable}
            onChange={(v) => set("resizable", v)}
            label={t("Resizable at runtime", "运行时可调整规格")}
          />
          {form.resizable && (
            <div className="mt-3 grid grid-cols-2 gap-3 border-t border-hairline pt-3">
              <Field label={t("Max memory MiB", "最大内存 MiB")} hint={t("Rounded up to 128 MiB slots.", "向上取整到 128 MiB 的槽位。")}>
                <input
                  className={inputCls}
                  type="number"
                  min={form.memory_mib}
                  step={128}
                  value={form.max_memory_mib}
                  onChange={(e) => set("max_memory_mib", Number(e.target.value))}
                />
              </Field>
              <Field label={t("Max vCPUs", "最大 vCPU")}>
                <input
                  className={inputCls}
                  type="number"
                  min={form.vcpus}
                  max={64}
                  value={form.max_vcpus}
                  onChange={(e) => set("max_vcpus", Number(e.target.value))}
                />
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

        <Field label={t("Egress", "出网")}>
          <select
            className={inputCls}
            value={form.egress}
            onChange={(e) => set("egress", e.target.value as "nat" | "none")}
          >
            <option value="nat">{t("nat — outbound internet", "nat — 可出公网")}</option>
            <option value="none">{t("none — no outbound network", "none — 无出网")}</option>
          </select>
        </Field>

        <ErrorNote error={create.error} />
        <div className="flex justify-end gap-2">
          <Button onClick={props.onClose}>{t("Cancel", "取消")}</Button>
          <Button kind="primary" type="submit" busy={create.isPending} disabled={ready.length === 0}>
            {t("Create sandbox", "创建沙箱")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
