// Parity cases mirror pkg/controlplane server.go validateGeometry /
// validateCeilings and the 128 MiB slot rounding — if the server rules
// move, these are the tests that should break.

import { describe, expect, it } from "vitest";
import type { TFn } from "./i18n";
import {
  effectiveCeiling,
  roundUpToSlot,
  slotRoundingHint,
  validateGeometry,
} from "./geometry";

const t: TFn = (en) => en;

describe("roundUpToSlot / effectiveCeiling", () => {
  it("rounds regions up to 128 MiB slots off a base", () => {
    // Mirrors server.go: base + roundUpToSlot(max-base).
    expect(effectiveCeiling(256, 1000)).toBe(1024); // 256 + 768
    expect(effectiveCeiling(256, 4096)).toBe(4096); // already aligned
    expect(effectiveCeiling(256, 257)).toBe(384); // one slot
    expect(effectiveCeiling(256, 256)).toBe(256); // no region
    expect(effectiveCeiling(256, 100)).toBe(256); // below base: clamp
    expect(roundUpToSlot(1)).toBe(128);
    expect(roundUpToSlot(128)).toBe(128);
  });

  it("hints only when rounding changes the value", () => {
    expect(slotRoundingHint(256, 1000, t)).toContain("1024");
    expect(slotRoundingHint(256, 1024, t)).toBeNull();
    expect(slotRoundingHint(256, 256, t)).toBeNull();
  });
});

describe("validateGeometry (server-rule parity)", () => {
  it("accepts the plain shapes", () => {
    expect(validateGeometry({}, t)).toBeNull();
    expect(validateGeometry({ vcpus: 1, memory_mib: 256 }, t)).toBeNull();
    expect(
      validateGeometry({ vcpus: 1, memory_mib: 256, max_vcpus: 4, max_memory_mib: 4096, autoscale: true }, t),
    ).toBeNull();
  });

  it("rejects out-of-range values (server.go validateGeometry bounds)", () => {
    expect(validateGeometry({ vcpus: 65 }, t)).not.toBeNull();
    expect(validateGeometry({ memory_mib: (1 << 20) + 1 }, t)).not.toBeNull();
    expect(validateGeometry({ data_disk_gib: 4097 }, t)).not.toBeNull();
  });

  it("rejects ceilings below the base (validateCeilings)", () => {
    expect(validateGeometry({ memory_mib: 512, max_memory_mib: 256 }, t)).not.toBeNull();
    expect(validateGeometry({ vcpus: 4, max_vcpus: 2 }, t)).not.toBeNull();
    expect(validateGeometry({ max_memory_mib: (1 << 20) + 1 }, t)).not.toBeNull();
    expect(validateGeometry({ max_vcpus: 65 }, t)).not.toBeNull();
  });

  it("rejects autoscale on an explicit fixed base (M7 rule)", () => {
    expect(validateGeometry({ memory_mib: 512, autoscale: true }, t)).not.toBeNull();
    // …but not when the base is omitted (default-elastic) or a ceiling exists.
    expect(validateGeometry({ autoscale: true }, t)).toBeNull();
    expect(validateGeometry({ memory_mib: 512, max_memory_mib: 1024, autoscale: true }, t)).toBeNull();
  });
});
