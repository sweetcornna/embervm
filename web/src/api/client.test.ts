import { describe, expect, it } from "vitest";
import { decodeBytes, fmtAge, fmtBytes, fmtMiB } from "./client";

describe("formatters", () => {
  it("fmtMiB keeps sub-GiB and odd sizes in MiB", () => {
    expect(fmtMiB(256)).toBe("256 MiB");
    expect(fmtMiB(1000)).toBe("1000 MiB");
  });
  it("fmtMiB promotes clean GiB multiples", () => {
    expect(fmtMiB(1024)).toBe("1 GiB");
    expect(fmtMiB(1536)).toBe("1.50 GiB");
  });
  it("fmtBytes scales binary units", () => {
    expect(fmtBytes(0)).toBe("0 B");
    expect(fmtBytes(1024)).toBe("1.0 KiB");
    expect(fmtBytes(3 * 1024 * 1024 * 1024)).toBe("3.0 GiB");
  });
  it("fmtAge buckets by unit", () => {
    const at = (ms: number) => new Date(Date.now() - ms).toISOString();
    expect(fmtAge(at(30_000))).toBe("30s");
    expect(fmtAge(at(5 * 60_000))).toBe("5m");
    expect(fmtAge(at(3 * 3_600_000))).toBe("3h");
  });
});

describe("decodeBytes", () => {
  it("decodes Go []byte base64 to text", () => {
    expect(decodeBytes(btoa("hello ember"))).toBe("hello ember");
  });
  it("tolerates empty and garbage input", () => {
    expect(decodeBytes(undefined)).toBe("");
    expect(decodeBytes("not-base64!!")).toBe("not-base64!!");
  });
});
