import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router-dom";
import { fmtAge } from "../api/client";
import { useSandboxes, useTemplates, verbs } from "../api/hooks";
import type { Template } from "../api/types";
import {
  Button,
  Card,
  ConfirmDialog,
  Drawer,
  Empty,
  ErrorNote,
  Field,
  Mono,
  PageHeader,
  Table,
  inputCls,
} from "../components/ui";
import { toast } from "../lib/toast";

const TSTATE: Record<string, string> = {
  READY: "var(--color-ok)",
  BUILDING: "var(--color-transit)",
  ERROR: "var(--color-danger)",
};

export function Templates() {
  const { data, isLoading } = useTemplates();
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [image, setImage] = useState("");
  const [detail, setDetail] = useState<Template | null>(null);
  const [confirmDel, setConfirmDel] = useState<Template | null>(null);

  const build = useMutation({
    mutationFn: () => verbs.createTemplate(name.trim(), image.trim()),
    onSuccess: () => {
      setName("");
      setImage("");
      void qc.invalidateQueries({ queryKey: ["templates"] });
    },
  });
  const del = useMutation({
    mutationFn: (id: string) => verbs.deleteTemplate(id),
    onSuccess: () => toast.success("Template deleted"),
    onError: (err) => toast.error("Delete failed", err.message),
    onSettled: () => void qc.invalidateQueries({ queryKey: ["templates"] }),
  });

  return (
    <div className="space-y-5">
      <PageHeader
        title="Templates"
        subtitle="A template turns a container image into a bootable microVM root; sandboxes clone it in O(1)."
      />

      <Card title="Build a template">
        <form
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => {
            e.preventDefault();
            if (name.trim() && image.trim()) build.mutate();
          }}
        >
          <div className="w-44 grow-0">
            <Field label="Name">
              <input className={inputCls} value={name} onChange={(e) => setName(e.target.value)} placeholder="web" />
            </Field>
          </div>
          <div className="min-w-52 grow">
            <Field label="Container image">
              <input
                className={inputCls}
                value={image}
                onChange={(e) => setImage(e.target.value)}
                placeholder="alpine:3.20"
              />
            </Field>
          </div>
          <Button kind="primary" type="submit" busy={build.isPending} disabled={!name.trim() || !image.trim()}>
            Build
          </Button>
        </form>
        {(build.error || del.error) && (
          <div className="mt-3">
            <ErrorNote error={build.error ?? del.error} />
          </div>
        )}
      </Card>

      <Table head={["Name", "Image", "State", "Age", ""]}>
        {(data ?? []).map((t) => (
          <tr key={t.id} className="border-b border-hairline last:border-0 hover:bg-raised/40">
            <td className="px-4 py-2.5">
              <button className="font-medium text-ink hover:text-accent" onClick={() => setDetail(t)}>
                {t.name}
              </button>
            </td>
            <td className="px-4 py-2.5">
              <Mono className="text-muted">{t.image}</Mono>
            </td>
            <td className="px-4 py-2.5">
              <span
                className="inline-flex items-center gap-1.5 font-mono text-xs"
                style={{ color: TSTATE[t.state] ?? "var(--color-idle)" }}
              >
                <span
                  className="inline-block size-1.5 rounded-full"
                  style={{ background: TSTATE[t.state] ?? "var(--color-idle)" }}
                />
                {t.state.toLowerCase()}
              </span>
              {t.error && <span className="ml-2 font-mono text-[11px] text-danger">{t.error}</span>}
            </td>
            <td className="px-4 py-2.5">
              <Mono className="text-muted tabular-nums">{fmtAge(t.created_at)}</Mono>
            </td>
            <td className="px-4 py-2.5 text-right">
              <Button size="sm" kind="danger" onClick={() => setConfirmDel(t)}>
                Delete
              </Button>
            </td>
          </tr>
        ))}
      </Table>
      {!isLoading && (data ?? []).length === 0 && (
        <Empty>No templates. Build one from a container image above.</Empty>
      )}
      {isLoading && <Empty>Loading…</Empty>}

      <Drawer title={detail ? `Template ${detail.name}` : "Template"} open={detail !== null} onClose={() => setDetail(null)}>
        {detail && <TemplateDetail template={detail} onDelete={() => setConfirmDel(detail)} />}
      </Drawer>
      <ConfirmDialog
        open={confirmDel !== null}
        title="Delete template"
        body={
          <>
            Delete <Mono className="text-ink">{confirmDel?.name}</Mono>? Sandboxes already built from
            it keep running; new sandboxes can no longer use it.
          </>
        }
        confirmLabel="Delete template"
        busy={del.isPending}
        onConfirm={() => {
          if (confirmDel) del.mutate(confirmDel.id);
          setConfirmDel(null);
          setDetail(null);
        }}
        onClose={() => setConfirmDel(null)}
      />
    </div>
  );
}

function TemplateDetail(props: { template: Template; onDelete: () => void }) {
  const { template: t } = props;
  const sandboxes = useSandboxes();
  const usedBy = (sandboxes.data ?? []).filter((sb) => sb.template_id === t.id);
  const rows: Array<[string, string]> = [
    ["id", t.id],
    ["image", t.image],
    ["state", t.state.toLowerCase()],
    ["created", `${fmtAge(t.created_at)} ago`],
    ...(t.ready_at ? ([["ready", `${fmtAge(t.ready_at)} ago`]] as Array<[string, string]>) : []),
  ];
  return (
    <div className="space-y-5">
      <dl className="grid grid-cols-1 gap-2.5">
        {rows.map(([k, v]) => (
          <div key={k} className="flex justify-between gap-3">
            <dt className="font-mono text-[10px] uppercase tracking-[0.12em] text-faint">{k}</dt>
            <dd className="min-w-0 break-all text-right font-mono text-[12px] text-ink">{v}</dd>
          </div>
        ))}
      </dl>
      {t.error && <ErrorNote error={new Error(t.error)} />}
      <div>
        <h3 className="mb-2 font-mono text-[10px] uppercase tracking-[0.14em] text-faint">
          Sandboxes using it ({usedBy.length})
        </h3>
        {usedBy.length === 0 ? (
          <Empty>None yet.</Empty>
        ) : (
          <ul className="divide-y divide-hairline overflow-hidden rounded-md border border-hairline">
            {usedBy.map((sb) => (
              <li key={sb.id} className="px-3 py-2">
                <Link to={`/sandboxes/${sb.id}`} className="font-mono text-[12px] hover:text-accent">
                  {sb.id.slice(0, 8)}
                </Link>
                <span className="ml-2 font-mono text-[11px] text-faint">{sb.state.toLowerCase()}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
      <Button kind="danger" onClick={props.onDelete}>
        Delete template
      </Button>
    </div>
  );
}
