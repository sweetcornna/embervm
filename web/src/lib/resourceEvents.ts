// The single parser/formatter for M7 resource events (resize / migrate /
// autoscale_config) riding sandbox_events.detail. Three surfaces render
// them (workspace timeline, scaling-activity card, fleet feed) — they all
// go through here so a backend shape change breaks one module, and unknown
// kinds fall back to the generic lifecycle row (return null).

import { fmtMiB } from "../api/client";
import type { ResourceEventDetail, SandboxEvent } from "../api/types";
import type { TFn } from "./i18n";

export const RESOURCE_EVENT_KINDS = new Set(["resize", "migrate", "autoscale_config"]);

/** Returns the typed resource detail when the event is one of ours, else
    null (lifecycle transitions, error details, future kinds). */
export function parseResourceEvent(ev: SandboxEvent): ResourceEventDetail | null {
  const d = ev.detail as ResourceEventDetail | undefined;
  if (!d || typeof d.kind !== "string" || !RESOURCE_EVENT_KINDS.has(d.kind)) return null;
  return d;
}

export interface ResourceEventView {
  /** grow | shrink | migrate | config | deferred — drives icon + tone */
  icon: "grow" | "shrink" | "migrate" | "config" | "deferred";
  text: string;
  /** ok | warn — deferred growth is the honest "node is full" surface */
  tone: "ok" | "warn";
  /** user | autoscale chip */
  actor?: string;
}

function pairText(pair: [number, number] | undefined, fmt: (n: number) => string): string | null {
  if (!pair) return null;
  return `${fmt(pair[0])} → ${fmt(pair[1])}`;
}

/** One-line human description, bilingual via t. */
export function describeResourceEvent(d: ResourceEventDetail, t: TFn): ResourceEventView {
  const actor = d.actor === "autoscale" ? t("autoscale", "自动伸缩") : d.actor === "user" ? t("user", "用户") : d.actor;
  switch (d.kind) {
    case "resize": {
      if (d.reason === "deferred") {
        return {
          icon: "deferred",
          tone: "warn",
          actor,
          text: t(
            "Growth deferred — the node is out of budget; retrying, or migrate to a roomier node",
            "扩容被推迟——节点预算已满；将自动重试，或迁移到更宽裕的节点",
          ),
        };
      }
      const mem = pairText(d.memory_mib, fmtMiB);
      const cpu = pairText(d.vcpus, (n) => `${n} vCPU`);
      const moved = [mem, cpu].filter(Boolean).join(" · ");
      const grew =
        (d.memory_mib && d.memory_mib[1] > d.memory_mib[0]) ||
        (d.vcpus && d.vcpus[1] > d.vcpus[0]);
      return {
        icon: grew ? "grow" : "shrink",
        tone: "ok",
        actor,
        text: (grew ? t("Grew", "扩容") : t("Shrank", "缩容")) + (moved ? ` ${moved}` : ""),
      };
    }
    case "migrate":
      return {
        icon: "migrate",
        tone: "ok",
        actor,
        text: t("Migrated", "迁移") + ` ${d.from_node ?? "?"} → ${d.to_node ?? "?"}`,
      };
    case "autoscale_config":
      return {
        icon: "config",
        tone: "ok",
        actor,
        text: d.enabled
          ? t("Autoscale turned on", "自动伸缩已开启")
          : t("Autoscale turned off", "自动伸缩已关闭"),
      };
    default:
      return { icon: "config", tone: "ok", actor, text: d.kind };
  }
}
