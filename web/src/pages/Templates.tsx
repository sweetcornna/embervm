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
import { useI18n } from "../lib/i18n";
import { toast } from "../lib/toast";

const TSTATE: Record<string, string> = {
  READY: "var(--color-ok)",
  BUILDING: "var(--color-transit)",
  ERROR: "var(--color-danger)",
};

export function Templates() {
  const { t } = useI18n();
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
    onSuccess: () => toast.success(t("Template deleted", "模板已删除")),
    onError: (err) => toast.error(t("Delete failed", "删除失败"), err.message),
    onSettled: () => void qc.invalidateQueries({ queryKey: ["templates"] }),
  });

  return (
    <div className="space-y-5">
      <PageHeader
        title={t("Templates", "模板")}
        subtitle={t(
          "A template turns a container image into a bootable microVM root; sandboxes clone it in O(1).",
          "模板把容器镜像变成可启动的 microVM 根文件系统；沙箱以 O(1) 克隆它。",
        )}
      />

      <Card title={t("Build a template", "构建模板")}>
        <form
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => {
            e.preventDefault();
            if (name.trim() && image.trim()) build.mutate();
          }}
        >
          <div className="w-44 grow-0">
            <Field label={t("Name", "名称")}>
              <input className={inputCls} value={name} onChange={(e) => setName(e.target.value)} placeholder="web" />
            </Field>
          </div>
          <div className="min-w-52 grow">
            <Field label={t("Container image", "容器镜像")}>
              <input
                className={inputCls}
                value={image}
                onChange={(e) => setImage(e.target.value)}
                placeholder="alpine:3.20"
              />
            </Field>
          </div>
          <Button kind="primary" type="submit" busy={build.isPending} disabled={!name.trim() || !image.trim()}>
            {t("Build", "构建")}
          </Button>
        </form>
        {(build.error || del.error) && (
          <div className="mt-3">
            <ErrorNote error={build.error ?? del.error} />
          </div>
        )}
      </Card>

      <Table head={[t("Name", "名称"), t("Image", "镜像"), t("State", "状态"), t("Age", "时长"), ""]}>
        {(data ?? []).map((tpl) => (
          <tr key={tpl.id} className="border-b border-hairline last:border-0 hover:bg-raised/40">
            <td className="px-4 py-2.5">
              <button className="font-medium text-ink hover:text-accent" onClick={() => setDetail(tpl)}>
                {tpl.name}
              </button>
            </td>
            <td className="px-4 py-2.5">
              <Mono className="text-muted">{tpl.image}</Mono>
            </td>
            <td className="px-4 py-2.5">
              <span
                className="inline-flex items-center gap-1.5 font-mono text-xs"
                style={{ color: TSTATE[tpl.state] ?? "var(--color-idle)" }}
              >
                <span
                  className="inline-block size-1.5 rounded-full"
                  style={{ background: TSTATE[tpl.state] ?? "var(--color-idle)" }}
                />
                {tpl.state.toLowerCase()}
              </span>
              {tpl.error && <span className="ml-2 font-mono text-[11px] text-danger">{tpl.error}</span>}
            </td>
            <td className="px-4 py-2.5">
              <Mono className="text-muted tabular-nums">{fmtAge(tpl.created_at)}</Mono>
            </td>
            <td className="px-4 py-2.5 text-right">
              <Button size="sm" kind="danger" onClick={() => setConfirmDel(tpl)}>
                {t("Delete", "删除")}
              </Button>
            </td>
          </tr>
        ))}
      </Table>
      {!isLoading && (data ?? []).length === 0 && (
        <Empty>{t("No templates. Build one from a container image above.", "暂无模板。用上方的容器镜像构建一个。")}</Empty>
      )}
      {isLoading && <Empty>{t("Loading…", "加载中…")}</Empty>}

      <Drawer title={detail ? `${t("Template", "模板")} ${detail.name}` : t("Template", "模板")} open={detail !== null} onClose={() => setDetail(null)}>
        {detail && <TemplateDetail template={detail} onDelete={() => setConfirmDel(detail)} />}
      </Drawer>
      <ConfirmDialog
        open={confirmDel !== null}
        title={t("Delete template", "删除模板")}
        body={
          <>
            {t("Delete", "删除")} <Mono className="text-ink">{confirmDel?.name}</Mono>
            {t(
              "? Sandboxes already built from it keep running; new sandboxes can no longer use it.",
              "？已用它构建的沙箱会继续运行；新沙箱将无法再使用它。",
            )}
          </>
        }
        confirmLabel={t("Delete template", "删除模板")}
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
  const { t } = useI18n();
  const { template: tpl } = props;
  const sandboxes = useSandboxes();
  const usedBy = (sandboxes.data ?? []).filter((sb) => sb.template_id === tpl.id);
  const rows: Array<[string, string]> = [
    ["id", tpl.id],
    ["image", tpl.image],
    ["state", tpl.state.toLowerCase()],
    ["created", `${fmtAge(tpl.created_at)} ago`],
    ...(tpl.ready_at ? ([["ready", `${fmtAge(tpl.ready_at)} ago`]] as Array<[string, string]>) : []),
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
      {tpl.error && <ErrorNote error={new Error(tpl.error)} />}
      <div>
        <h3 className="mb-2 font-mono text-[10px] uppercase tracking-[0.14em] text-faint">
          {t("Sandboxes using it", "使用它的沙箱")} ({usedBy.length})
        </h3>
        {usedBy.length === 0 ? (
          <Empty>{t("None yet.", "暂无。")}</Empty>
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
        {t("Delete template", "删除模板")}
      </Button>
    </div>
  );
}
