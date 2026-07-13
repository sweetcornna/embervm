import { describe, expect, it } from "vitest";
import { baseName, joinPath, languageOf, looksBinary, normalizePath, parentPath } from "./files";

describe("guest path helpers", () => {
  it("normalizes", () => {
    expect(normalizePath("/a/b/../c//d/.")).toBe("/a/c/d");
    expect(normalizePath("/../..")).toBe("/");
    expect(normalizePath("/")).toBe("/");
  });
  it("joins", () => {
    expect(joinPath("/", "etc")).toBe("/etc");
    expect(joinPath("/etc/", "hosts")).toBe("/etc/hosts");
    expect(joinPath("/a/b", "../c")).toBe("/a/c");
  });
  it("parent and base", () => {
    expect(parentPath("/a/b/c")).toBe("/a/b");
    expect(parentPath("/a")).toBe("/");
    expect(parentPath("/")).toBe("/");
    expect(baseName("/a/b/c.txt")).toBe("c.txt");
  });
});

describe("looksBinary", () => {
  it("flags NUL in the head", () => {
    expect(looksBinary(new Uint8Array([104, 105, 0, 33]))).toBe(true);
  });
  it("passes utf-8 text", () => {
    expect(looksBinary(new TextEncoder().encode("hello 世界\nline2"))).toBe(false);
  });
  it("only sniffs the first 8 KiB", () => {
    const big = new Uint8Array(10_000).fill(65);
    big[9_500] = 0; // beyond the sniff window
    expect(looksBinary(big)).toBe(false);
  });
  it("empty file is text", () => {
    expect(looksBinary(new Uint8Array())).toBe(false);
  });
});

describe("languageOf", () => {
  it.each([
    ["main.ts", "javascript"],
    ["app.jsx", "javascript"],
    ["train.py", "python"],
    ["config.yaml", "yaml"],
    ["config.yml", "yaml"],
    ["run.sh", "shell"],
    ["Dockerfile", "dockerfile"],
    ["dockerfile.prod", "dockerfile"],
    ["Makefile", "shell"],
    ["main.go", "go"],
    ["notes.md", "markdown"],
    ["data.json", "json"],
    ["index.html", "html"],
    ["style.css", "css"],
    ["README", ""],
    ["archive.tar.gz", ""],
  ])("%s → %s", (name, want) => {
    expect(languageOf(name)).toBe(want);
  });
});
