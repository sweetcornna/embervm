import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { fmtAge } from "../api/client";
import { useTemplates, verbs } from "../api/hooks";
import { Button, Card, Empty, ErrorNote, Field, Mono, PageHeader, Table, inputCls } from "../components/ui";

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
            <td className="px-4 py-2.5 font-medium text-ink">{t.name}</td>
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
              <Button
                size="sm"
                kind="danger"
                onClick={() => {
                  if (window.confirm(`Delete template "${t.name}"?`)) del.mutate(t.id);
                }}
              >
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
    </div>
  );
}
