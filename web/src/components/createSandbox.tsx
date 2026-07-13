// The one "New sandbox" dialog — shared by the Sandboxes page, the Overview
// empty state, and (later) the command palette. Creates and navigates into
// the new workspace.

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useTemplates, verbs } from "../api/hooks";
import type { CreateSandboxRequest } from "../api/types";
import { Button, Dialog, ErrorNote, Field, Toggle, inputCls } from "./ui";

export function CreateSandboxDialog(props: { open: boolean; onClose: () => void }) {
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
    <Dialog title="New sandbox" open={props.open} onClose={props.onClose}>
      <form onSubmit={submit} className="space-y-4">
        <Field label="Template">
          <select
            className={inputCls}
            value={form.template_id || ready[0]?.id || ""}
            onChange={(e) => set("template_id", e.target.value)}
          >
            {ready.length === 0 && <option value="">No READY templates — build one first</option>}
            {ready.map((t) => (
              <option key={t.id} value={t.id}>
                {t.name} ({t.image})
              </option>
            ))}
          </select>
        </Field>
        <div className="grid grid-cols-3 gap-3">
          <Field label="vCPUs">
            <input
              className={inputCls}
              type="number"
              min={1}
              max={64}
              value={form.vcpus}
              onChange={(e) => set("vcpus", Number(e.target.value))}
            />
          </Field>
          <Field label="Memory MiB">
            <input
              className={inputCls}
              type="number"
              min={128}
              step={128}
              value={form.memory_mib}
              onChange={(e) => set("memory_mib", Number(e.target.value))}
            />
          </Field>
          <Field label="Disk GiB">
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
            label="Resizable at runtime"
          />
          {form.resizable && (
            <div className="mt-3 grid grid-cols-2 gap-3 border-t border-hairline pt-3">
              <Field label="Max memory MiB" hint="Rounded up to 128 MiB slots.">
                <input
                  className={inputCls}
                  type="number"
                  min={form.memory_mib}
                  step={128}
                  value={form.max_memory_mib}
                  onChange={(e) => set("max_memory_mib", Number(e.target.value))}
                />
              </Field>
              <Field label="Max vCPUs">
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
                  label="Autoscale on guest pressure"
                />
              </div>
            </div>
          )}
        </div>

        <Field label="Egress">
          <select
            className={inputCls}
            value={form.egress}
            onChange={(e) => set("egress", e.target.value as "nat" | "none")}
          >
            <option value="nat">nat — outbound internet</option>
            <option value="none">none — no outbound network</option>
          </select>
        </Field>

        <ErrorNote error={create.error} />
        <div className="flex justify-end gap-2">
          <Button onClick={props.onClose}>Cancel</Button>
          <Button kind="primary" type="submit" busy={create.isPending} disabled={ready.length === 0}>
            Create sandbox
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
