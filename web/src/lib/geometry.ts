// Client-side mirror of the control plane's geometry rules (M7):
// pkg/controlplane server.go validateGeometry/validateCeilings and the
// 128 MiB hotplug-slot rounding, plus the elastic ceiling presets the
// create dialog offers. Validation here is a UX nicety — the server remains
// the enforcement point.

import type { TFn } from "./i18n";

/** Firecracker's virtio-mem KVM slot granularity (pkg/nodeagent). */
export const HOTPLUG_SLOT_MIB = 128;

export const MAX_VCPUS = 64;
export const MAX_MEMORY_MIB = 1 << 20; // 1 TiB
export const MAX_DISK_GIB = 4096;

/** The platform defaults resolveGeometry applies to a no-geometry create.
    Display copy only — the server's response is the truth after create. */
export const DEFAULT_BASE: { vcpus: number; memory_mib: number } = { vcpus: 1, memory_mib: 256 };
export const DEFAULT_CEILING: { vcpus: number; memory_mib: number } = { vcpus: 4, memory_mib: 4096 };

export function roundUpToSlot(n: number): number {
  return Math.ceil(n / HOTPLUG_SLOT_MIB) * HOTPLUG_SLOT_MIB;
}

/** The ceiling the server will actually store: base + slot-rounded region
    (mirrors createSandbox's rounding). */
export function effectiveCeiling(baseMiB: number, maxMiB: number): number {
  if (maxMiB <= baseMiB) return baseMiB;
  return baseMiB + roundUpToSlot(maxMiB - baseMiB);
}

export interface ElasticPreset {
  key: string;
  label: string; // English
  zh: string;
  max_memory_mib?: number; // undefined = platform default (send nothing)
  max_vcpus?: number;
}

/** Ceiling presets for the create dialog's elastic mode. "standard" sends
    no ceilings at all — the server default is the contract. */
export const ELASTIC_PRESETS: ElasticPreset[] = [
  { key: "standard", label: "Standard", zh: "标准" }, // server default 4 GiB / 4 vCPU
  { key: "small", label: "Small", zh: "小型", max_memory_mib: 1024, max_vcpus: 1 },
  { key: "large", label: "Large", zh: "大型", max_memory_mib: 8192, max_vcpus: 8 },
];

export interface GeometryInput {
  vcpus?: number;
  memory_mib?: number;
  data_disk_gib?: number;
  max_vcpus?: number;
  max_memory_mib?: number;
  autoscale?: boolean;
}

/** Mirrors validateGeometry + validateCeilings + the autoscale-needs-ceiling
    rule for ADVANCED mode (explicit fields). Returns a bilingual error via t,
    or null when valid. Fields left undefined/0 are "server default". */
export function validateGeometry(g: GeometryInput, t: TFn): string | null {
  const { vcpus = 0, memory_mib = 0, data_disk_gib = 0, max_vcpus = 0, max_memory_mib = 0 } = g;
  if (vcpus < 0 || vcpus > MAX_VCPUS) {
    return t(`vCPUs must be within 1–${MAX_VCPUS}`, `vCPU 须在 1–${MAX_VCPUS} 之间`);
  }
  if (memory_mib < 0 || memory_mib > MAX_MEMORY_MIB) {
    return t("Memory must be within 1 MiB – 1 TiB", "内存须在 1 MiB – 1 TiB 之间");
  }
  if (data_disk_gib < 0 || data_disk_gib > MAX_DISK_GIB) {
    return t(`Disk must be within 1–${MAX_DISK_GIB} GiB`, `磁盘须在 1–${MAX_DISK_GIB} GiB 之间`);
  }
  if (max_memory_mib !== 0 && memory_mib !== 0 && max_memory_mib < memory_mib) {
    return t("Memory ceiling must be ≥ base memory", "内存上限须不低于基础内存");
  }
  if (max_memory_mib !== 0 && max_memory_mib > MAX_MEMORY_MIB) {
    return t("Memory ceiling exceeds 1 TiB", "内存上限超过 1 TiB");
  }
  if (max_vcpus !== 0 && vcpus !== 0 && max_vcpus < vcpus) {
    return t("vCPU ceiling must be ≥ base vCPUs", "vCPU 上限须不低于基础 vCPU");
  }
  if (max_vcpus !== 0 && max_vcpus > MAX_VCPUS) {
    return t(`vCPU ceiling exceeds ${MAX_VCPUS}`, `vCPU 上限超过 ${MAX_VCPUS}`);
  }
  // Explicit base without any ceiling cannot autoscale (fixed geometry).
  const baseGiven = vcpus > 0 || memory_mib > 0;
  const maxGiven = max_vcpus > 0 || max_memory_mib > 0;
  if (g.autoscale && baseGiven && !maxGiven) {
    return t(
      "Autoscale needs a ceiling — set max memory/vCPUs, or leave the base empty for the platform default",
      "自动伸缩需要上限——请设置内存/vCPU 上限，或不填基础规格以使用平台默认",
    );
  }
  return null;
}

/** "rounds up to N MiB" hint when the picked ceiling is off the slot grid. */
export function slotRoundingHint(baseMiB: number, maxMiB: number, t: TFn): string | null {
  if (maxMiB <= baseMiB) return null;
  const eff = effectiveCeiling(baseMiB, maxMiB);
  if (eff === maxMiB) return null;
  return t(`Ceiling rounds up to ${eff} MiB (128 MiB slots)`, `上限将取整到 ${eff} MiB（128 MiB 粒度）`);
}
