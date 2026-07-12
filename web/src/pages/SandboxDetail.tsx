import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { decodeBytes, fmtAge, fmtBytes, fmtMiB } from "../api/client";
import { useCheckpoints, useSandbox, useSandboxAction, useStorage, verbs } from "../api/hooks";
import type { ExecResponse, Sandbox } from "../api/types";
import { MemGauge, StateMark } from "../components/ember";
import { Button, Card, Empty, ErrorNote, Field, Mono, inputCls } from "../components/ui";

function Lifecycle(props: { sb: Sandbox }) {
  const { sb } = props;
  const nav = useNavigate();
  const pause = useSandboxAction(() => verbs.pause(sb.id));
  const resume = useSandboxAction(() => verbs.resume(sb.id));
  const snapshot = useSandboxAction(() => verbs.snapshot(sb.id, "console"));
  const migrate = useSandboxAction(() => verbs.migrate(sb.id));
  const kill = useMutation({
    mutationFn: () => verbs.kill(sb.id),
    onSuccess: () => nav("/sandboxes"),
  });
  const running = sb.state === "RUNNING";
  const paused = sb.state.startsWith("PAUSED") || sb.state === "ARCHIVED_COLD" || sb.state === "FAILED";
  const err = pause.error ?? resume.error ?? snapshot.error ?? migrate.error ?? kill.error;
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap gap-2">
        <Button onClick={() => pause.mutate()} busy={pause.isPending} disabled={!running}>
          Pause
        </Button>
        <Button onClick={() => resume.mutate()} busy={resume.isPending} disabled={!paused}>
          Resume
        </Button>
        <Button
          onClick={() => snapshot.mutate()}
          busy={snapshot.isPending}
          disabled={!running}
          title="Pause → checkpoint → resume"
        >
          Snapshot
        </Button>
        <Button
          onClick={() => migrate.mutate()}
          busy={migrate.isPending}
          disabled={!running && sb.state !== "PAUSED_HOT"}
          title="Move to another node (running: ~seconds pause; paused: pointer move)"
        >
          Migrate
        </Button>
        <Button
          kind="danger"
          onClick={() => {
            if (window.confirm(`Kill sandbox ${sb.id.slice(0, 8)}? Its disk and snapshots are destroyed.`))
              kill.mutate();
          }}
          busy={kill.isPending}
        >
          Kill
        </Button>
      </div>
      <ErrorNote error={err} />
    </div>
  );
}

function ResizePanel(props: { sb: Sandbox }) {
  const { sb } = props;
  const base = sb.base_memory_mib || sb.memory_mib;
  const maxMem = sb.max_memory_mib ?? 0;
  const maxCPU = sb.max_vcpus ?? 0;
  const resizable = maxMem > base || maxCPU > (sb.base_vcpus || sb.vcpus);
  const [mem, setMem] = useState(sb.memory_mib);
  const [cpu, setCPU] = useState(sb.vcpus);
  const resize = useSandboxAction(() =>
    verbs.resize(sb.id, {
      memory_mib: mem !== sb.memory_mib ? mem : undefined,
      vcpus: cpu !== sb.vcpus ? cpu : undefined,
    }),
  );

  if (!resizable)
    return (
      <p className="text-sm text-faint">
        Fixed geometry — create with <Mono>max_memory_mib</Mono> / <Mono>max_vcpus</Mono> to enable
        runtime resize.
      </p>
    );

  const dirty = mem !== sb.memory_mib || cpu !== sb.vcpus;
  return (
    <div className="space-y-4">
      <MemGauge
        wide
        state={sb.state}
        memoryMiB={sb.memory_mib}
        baseMiB={sb.base_memory_mib}
        maxMiB={sb.max_memory_mib}
      />
      <div className="grid gap-4 sm:grid-cols-2">
        {maxMem > base && (
          <Field label={`Memory · ${fmtMiB(base)} – ${fmtMiB(maxMem)}`}>
            <div className="flex items-center gap-3">
              <input
                type="range"
                min={base}
                max={maxMem}
                step={128}
                value={mem}
                onChange={(e) => setMem(Number(e.target.value))}
                className="w-full accent-(--color-ember)"
              />
              <Mono className="w-20 text-right">{fmtMiB(mem)}</Mono>
            </div>
          </Field>
        )}
        {maxCPU > 0 && (
          <Field label={`vCPUs · 1 – ${maxCPU}`}>
            <input
              className={inputCls}
              type="number"
              min={1}
              max={maxCPU}
              value={cpu}
              onChange={(e) => setCPU(Number(e.target.value))}
            />
          </Field>
        )}
      </div>
      <div className="flex items-center gap-3">
        <Button
          kind="primary"
          onClick={() => resize.mutate()}
          busy={resize.isPending}
          disabled={!dirty || sb.state !== "RUNNING"}
        >
          Apply resize
        </Button>
        {sb.autoscale && (
          <span className="font-mono text-xs text-transit">
            autoscale on — the engine also moves these on guest pressure
          </span>
        )}
        {sb.state !== "RUNNING" && <span className="text-xs text-faint">resize needs RUNNING</span>}
      </div>
      <ErrorNote error={resize.error} />
    </div>
  );
}

function Checkpoints(props: { sb: Sandbox }) {
  const { sb } = props;
  const nav = useNavigate();
  const qc = useQueryClient();
  const { data, isLoading } = useCheckpoints(sb.id);
  const [tag, setTag] = useState("");
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ["sandboxes"] });
    void qc.invalidateQueries({ queryKey: ["sandboxes", sb.id, "checkpoints"] });
  };
  const checkpoint = useMutation({
    mutationFn: () => verbs.checkpoint(sb.id, tag.trim() || undefined),
    onSuccess: () => {
      setTag("");
      invalidate();
    },
  });
  const fork = useMutation({
    mutationFn: (cp?: string) => verbs.fork(sb.id, cp),
    onSuccess: (child) => nav(`/sandboxes/${child.id}`),
  });
  const rollback = useMutation({
    mutationFn: (cp: string) => verbs.rollback(sb.id, cp),
    onSuccess: invalidate,
  });
  const err = checkpoint.error ?? fork.error ?? rollback.error;

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-end gap-2">
        <div className="grow">
          <Field label="New checkpoint tag" hint="Blank = auto-named cp<seq>.">
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
          Checkpoint now
        </Button>
        <Button onClick={() => fork.mutate(undefined)} busy={fork.isPending} title="Checkpoint, then branch">
          Fork now
        </Button>
      </div>

      {isLoading && <Empty>Loading…</Empty>}
      {data && data.length === 0 && <Empty>No checkpoints yet — every one is a fork/rollback anchor.</Empty>}
      {data && data.length > 0 && (
        <ul className="divide-y divide-hairline rounded border border-hairline">
          {data.map((cp) => (
            <li key={cp.tag} className="flex items-center justify-between gap-3 px-3 py-2">
              <div>
                <Mono className="text-ink">{cp.tag}</Mono>
                <span className="ml-2 font-mono text-[11px] text-faint">
                  {cp.layer} · {fmtAge(cp.created_at)} ago
                </span>
              </div>
              <div className="flex gap-2">
                <Button onClick={() => fork.mutate(cp.tag)} busy={fork.isPending}>
                  Fork
                </Button>
                <Button
                  kind="danger"
                  onClick={() => {
                    if (window.confirm(`Roll back to "${cp.tag}"? Everything after it is discarded.`))
                      rollback.mutate(cp.tag);
                  }}
                  busy={rollback.isPending}
                >
                  Roll back
                </Button>
              </div>
            </li>
          ))}
        </ul>
      )}
      <ErrorNote error={err} />
    </div>
  );
}

function ExecPanel(props: { sb: Sandbox }) {
  const { sb } = props;
  const [cmdline, setCmdline] = useState("");
  const [result, setResult] = useState<ExecResponse | null>(null);
  const exec = useMutation({
    mutationFn: () => {
      const [cmd, ...args] = cmdline.trim().split(/\s+/);
      return verbs.exec(sb.id, cmd, args);
    },
    onSuccess: setResult,
  });
  return (
    <div className="space-y-3">
      <form
        className="flex gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (cmdline.trim()) exec.mutate();
        }}
      >
        <input
          className={`${inputCls} font-mono`}
          value={cmdline}
          onChange={(e) => setCmdline(e.target.value)}
          placeholder="uname -a"
          aria-label="Command"
        />
        <Button kind="primary" type="submit" busy={exec.isPending} disabled={sb.state !== "RUNNING"}>
          Run
        </Button>
      </form>
      <ErrorNote error={exec.error} />
      {result && (
        <div className="rounded border border-hairline bg-bg p-3">
          <div className="mb-2 font-mono text-[11px] text-muted">
            exit <span className={result.exit_code === 0 ? "text-ember" : "text-alarm"}>{result.exit_code}</span>
            {" · "}
            {result.duration_ms}ms
            {result.timed_out && <span className="text-alarm"> · timed out</span>}
            {result.truncated && <span className="text-transit"> · truncated</span>}
          </div>
          <pre className="max-h-72 overflow-auto whitespace-pre-wrap font-mono text-xs text-ink">
            {decodeBytes(result.stdout) || <span className="text-faint">(no stdout)</span>}
          </pre>
          {result.stderr && (
            <pre className="mt-2 max-h-40 overflow-auto whitespace-pre-wrap font-mono text-xs text-[#f1a3a6]">
              {decodeBytes(result.stderr)}
            </pre>
          )}
        </div>
      )}
    </div>
  );
}

function StorageCard(props: { id: string }) {
  const { data } = useStorage(props.id);
  if (!data) return <Empty>Loading…</Empty>;
  const rows: Array<[string, string]> = [
    ["tier", data.tier],
    ["logical", fmtBytes(data.logical_bytes)],
    ["stored", fmtBytes(data.stored_bytes)],
    ["stored / logical", data.logical_bytes > 0 ? `${(data.stored_ratio * 100).toFixed(1)}%` : "—"],
    ["chunks", String(data.chunk_count)],
    ["layers", String(data.layers)],
  ];
  return (
    <dl className="grid grid-cols-2 gap-x-4 gap-y-1.5 sm:grid-cols-3">
      {rows.map(([k, v]) => (
        <div key={k}>
          <dt className="font-mono text-[11px] uppercase tracking-wider text-muted">{k}</dt>
          <dd className="font-mono text-sm text-ink">{v}</dd>
        </div>
      ))}
    </dl>
  );
}

export function SandboxDetail() {
  const { id = "" } = useParams();
  const { data: sb, isLoading, error } = useSandbox(id);

  if (isLoading) return <Empty>Loading…</Empty>;
  if (error || !sb)
    return (
      <div className="mx-auto max-w-3xl space-y-3">
        <ErrorNote error={error ?? new Error("sandbox not found")} />
        <Link to="/sandboxes" className="text-sm text-ember hover:underline">
          ← back to sandboxes
        </Link>
      </div>
    );

  return (
    <div className="mx-auto max-w-4xl space-y-4">
      <header className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <h1 className="font-display text-xl font-bold tracking-wide">
          <Link to="/sandboxes" className="text-faint hover:text-muted">
            sandboxes /
          </Link>{" "}
          <Mono className="text-lg">{sb.id.slice(0, 8)}</Mono>
        </h1>
        <StateMark state={sb.state} />
        {sb.error && <span className="font-mono text-xs text-alarm">{sb.error}</span>}
      </header>

      <div className="flex flex-wrap gap-x-6 gap-y-1 font-mono text-xs text-muted">
        <span>node <span className="text-ink">{sb.node_id || "—"}</span></span>
        <span>template <span className="text-ink">{sb.template_id.slice(0, 8)}</span></span>
        <span>disk <span className="text-ink">{sb.data_disk_gib} GiB</span></span>
        <span>age <span className="text-ink">{fmtAge(sb.created_at)}</span></span>
        {sb.forked_from && <span>forked from <span className="text-ink">{sb.forked_from}</span></span>}
        {sb.autoscale && <span className="text-transit">autoscale</span>}
      </div>

      <Card title="Lifecycle">
        <Lifecycle sb={sb} />
      </Card>
      <Card title="Resources">
        <ResizePanel sb={sb} />
      </Card>
      <Card title="Checkpoints · fork · rollback">
        <Checkpoints sb={sb} />
      </Card>
      <Card title="Exec">
        <ExecPanel sb={sb} />
      </Card>
      <Card title="Storage">
        <StorageCard id={sb.id} />
      </Card>
    </div>
  );
}
