// @vitest-environment jsdom
import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const apiMock = vi.hoisted(() => vi.fn());
vi.mock("../api/client", () => ({ api: apiMock }));

import { resetHealthStore, useSandboxHealth } from "./health";

const RUNNING = {
  state: "RUNNING",
  ok: true,
  mem_total_kib: 1_000_000,
  mem_available_kib: 400_000,
  psi_mem_some10: 1.5,
  psi_cpu_some10: 0.2,
};

describe("health poller store", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    apiMock.mockReset();
    apiMock.mockResolvedValue(RUNNING);
  });
  afterEach(() => {
    resetHealthStore();
    vi.useRealTimers();
  });

  it("polls while subscribed and accumulates samples", async () => {
    const { result } = renderHook(() => useSandboxHealth("sb1"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0); // immediate first poll
    });
    expect(result.current.samples).toHaveLength(1);
    expect(result.current.latest?.health.ok).toBe(true);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_100); // two more ticks at 2.5s
    });
    expect(result.current.samples).toHaveLength(3);
    expect(apiMock).toHaveBeenCalledWith("GET", "/sandboxes/sb1/health");
  });

  it("marks unreachable on failure and clears on recovery", async () => {
    apiMock.mockRejectedValueOnce(new Error("502"));
    const { result } = renderHook(() => useSandboxHealth("sb1"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.unreachable).toBe(true);
    expect(result.current.samples).toHaveLength(0);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_600);
    });
    expect(result.current.unreachable).toBe(false);
    expect(result.current.samples).toHaveLength(1);
  });

  it("stops polling after the last unsubscribe", async () => {
    const { unmount } = renderHook(() => useSandboxHealth("sb1"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    const calls = apiMock.mock.calls.length;
    unmount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000);
    });
    expect(apiMock.mock.calls.length).toBe(calls);
  });

  it("two subscribers share one poll feed", async () => {
    const a = renderHook(() => useSandboxHealth("sb1"));
    const b = renderHook(() => useSandboxHealth("sb1"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_600);
    });
    expect(a.result.current.samples.length).toBe(b.result.current.samples.length);
    // One poller: ≤ 2 calls in ~2.6s (initial + one tick), not 4.
    expect(apiMock.mock.calls.length).toBeLessThanOrEqual(2);
    a.unmount();
    b.unmount();
  });
});
