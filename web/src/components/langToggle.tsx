// Sidebar language switch: EN | 中. Two-state segmented control.

import { useI18n } from "../lib/i18n";

export function LangToggle() {
  const { locale, setLocale } = useI18n();
  const opts: Array<{ value: "en" | "zh"; label: string }> = [
    { value: "en", label: "EN" },
    { value: "zh", label: "中" },
  ];
  return (
    <div
      role="group"
      aria-label="Language"
      className="inline-flex rounded-md border border-border p-0.5"
    >
      {opts.map((o) => (
        <button
          key={o.value}
          onClick={() => setLocale(o.value)}
          aria-pressed={locale === o.value}
          className={`rounded px-2 py-0.5 text-[11px] font-medium transition-colors ${
            locale === o.value ? "bg-raised text-ink" : "text-faint hover:text-ink"
          }`}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}
