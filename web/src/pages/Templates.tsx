import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { fmtAge } from "../api/client";
import { useTemplates, verbs } from "../api/hooks";
import { Button, Card, Empty, ErrorNote, Field, Mono, inputCls } from "../components/ui";

const TSTATE: Record<string, string> = {
  READY: "var(--color-ember)",
  BUILDING: "var(--color-transit)",
  ERROR: "var(--color-alarm)",
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
    <div className="mx-auto max-w-4xl space-y-4">
      <header>
        <h1 className="font-display text-2xl font-bold tracking-wide">Templates</h1>
        <p className="mt-1 text-sm text-muted">
          A template turns a container image into a bootable microVM root; sandboxes clone it in O(1).
        </p>
      </header>

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
          <div className="grow">
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
        <div className="mt-2">
          <ErrorNote error={build.error ?? del.error} />
        </div>
      </Card>

      <div className="overflow-x-auto rounded-md border border-border bg-surface">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-hairline text-left font-mono text-[11px] uppercase tracking-wider text-muted">
              <th className="px-4 py-2.5 font-medium">Name</th>
              <th className="px-4 py-2.5 font-medium">Image</th>
              <th className="px-4 py-2.5 font-medium">State</th>
              <th className="px-4 py-2.5 font-medium">Age</th>
              <th className="px-4 py-2.5" />
            </tr>
          </thead>
          <tbody>
            {(data ?? []).map((t) => (
              <tr key={t.id} className="border-b border-hairline last:border-0">
                <td className="px-4 py-2.5 font-medium">{t.name}</td>
                <td className="px-4 py-2.5">
                  <Mono className="text-muted">{t.image}</Mono>
                </td>
                <td className="px-4 py-2.5">
                  <span className="font-mono text-xs" style={{ color: TSTATE[t.state] ?? "var(--color-ash)" }}>
                    {t.state.toLowerCase()}
                  </span>
                  {t.error && <span className="ml-2 font-mono text-[11px] text-alarm">{t.error}</span>}
                </td>
                <td className="px-4 py-2.5">
                  <Mono className="text-muted">{fmtAge(t.created_at)}</Mono>
                </td>
                <td className="px-4 py-2.5 text-right">
                  <Button
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
          </tbody>
        </table>
        {!isLoading && (data ?? []).length === 0 && (
          <Empty>No templates. Build one from a container image above.</Empty>
        )}
      </div>
    </div>
  );
}
