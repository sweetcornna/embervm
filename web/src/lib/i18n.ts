// Bilingual (English / 简体中文) console. Deliberately dependency-free: a
// module-level locale + useSyncExternalStore, mirroring the toast/health
// stores. Strings are bilingual at the call site — `t("English", "中文")` —
// so translations live next to the code they label (no separate catalog to
// drift). Components call useI18n() to subscribe, so a locale switch
// re-renders everything.

import { useCallback, useSyncExternalStore } from "react";

export type Locale = "en" | "zh";

const STORAGE_KEY = "embervm.locale";

function initialLocale(): Locale {
  const saved = localStorage.getItem(STORAGE_KEY);
  if (saved === "en" || saved === "zh") return saved;
  // Follow the browser on first visit.
  return navigator.language?.toLowerCase().startsWith("zh") ? "zh" : "en";
}

let locale: Locale = initialLocale();
const listeners = new Set<() => void>();

export function getLocale(): Locale {
  return locale;
}

export function setLocale(next: Locale) {
  if (next === locale) return;
  locale = next;
  localStorage.setItem(STORAGE_KEY, next);
  document.documentElement.lang = next === "zh" ? "zh-CN" : "en";
  for (const l of listeners) l();
}

function subscribe(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/** Translate function: `t("Overview", "总览")`. Bound to the current locale. */
export type TFn = (en: string, zh: string) => string;

/** Subscribe to the locale and get a bound translate function. Every
    component rendering user-facing text should call this. */
export function useI18n(): { locale: Locale; t: TFn; setLocale: typeof setLocale } {
  const current = useSyncExternalStore(subscribe, getLocale, getLocale);
  const t = useCallback<TFn>((en, zh) => (current === "zh" ? zh : en), [current]);
  return { locale: current, t, setLocale };
}

// Keep <html lang> correct from the first paint.
document.documentElement.lang = locale === "zh" ? "zh-CN" : "en";
