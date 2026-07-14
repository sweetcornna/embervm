// The resource-event contract test: the parser/formatter three UI surfaces
// share. The detail shape is pinned against controlplane.ResourceEventDetail
// — a backend rename should break exactly this file.

import { describe, expect, it } from "vitest";
import type { SandboxEvent } from "../api/types";
import type { TFn } from "./i18n";
import { describeResourceEvent, parseResourceEvent } from "./resourceEvents";

const en: TFn = (e) => e;
const zh: TFn = (_, z) => z;

function ev(detail: Record<string, unknown> | undefined): SandboxEvent {
  return { id: 1, sandbox_id: "sb", to_state: "RUNNING", at: "2026-07-14T00:00:00Z", detail };
}

describe("parseResourceEvent", () => {
  it("accepts the three known kinds", () => {
    expect(parseResourceEvent(ev({ kind: "resize" }))).not.toBeNull();
    expect(parseResourceEvent(ev({ kind: "migrate" }))).not.toBeNull();
    expect(parseResourceEvent(ev({ kind: "autoscale_config" }))).not.toBeNull();
  });

  it("falls back to null for lifecycle rows, errors, and future kinds", () => {
    expect(parseResourceEvent(ev(undefined))).toBeNull();
    expect(parseResourceEvent(ev({ error: "boom" }))).toBeNull();
    expect(parseResourceEvent(ev({ kind: "gpu_attach" }))).toBeNull();
    expect(parseResourceEvent(ev({ kind: 42 as unknown as string }))).toBeNull();
  });
});

describe("describeResourceEvent", () => {
  it("describes a user grow with both dimensions", () => {
    const v = describeResourceEvent(
      { kind: "resize", actor: "user", reason: "manual", memory_mib: [256, 512], vcpus: [1, 2] },
      en,
    );
    expect(v.icon).toBe("grow");
    expect(v.tone).toBe("ok");
    expect(v.actor).toBe("user");
    expect(v.text).toContain("256 MiB → 512 MiB");
    expect(v.text).toContain("1 vCPU → 2 vCPU");
  });

  it("describes an autoscale shrink in Chinese", () => {
    const v = describeResourceEvent(
      { kind: "resize", actor: "autoscale", reason: "pressure", memory_mib: [512, 256] },
      zh,
    );
    expect(v.icon).toBe("shrink");
    expect(v.actor).toBe("自动伸缩");
    expect(v.text).toContain("缩容");
  });

  it("flags deferred growth as a warning", () => {
    const v = describeResourceEvent({ kind: "resize", actor: "autoscale", reason: "deferred" }, en);
    expect(v.icon).toBe("deferred");
    expect(v.tone).toBe("warn");
  });

  it("describes migrate endpoints and autoscale toggles", () => {
    const m = describeResourceEvent({ kind: "migrate", from_node: "n1", to_node: "n2" }, en);
    expect(m.icon).toBe("migrate");
    expect(m.text).toContain("n1 → n2");
    const on = describeResourceEvent({ kind: "autoscale_config", enabled: true }, zh);
    expect(on.text).toContain("已开启");
    const off = describeResourceEvent({ kind: "autoscale_config", enabled: false }, en);
    expect(off.text).toContain("turned off");
  });
});
