// Toast viewport — renders the lib/toast queue via Radix Toast (aria-live,
// swipe dismiss, focus hotkey). Mount once in App.

import { Toast as RadixToast } from "radix-ui";
import { dismiss, useToasts } from "../lib/toast";
import { IconCheck, IconClose, IconInfo, IconWarn } from "./icons";

const KIND_META = {
  success: { color: "var(--color-ok)", Icon: IconCheck },
  error: { color: "var(--color-danger)", Icon: IconWarn },
  info: { color: "var(--color-cold)", Icon: IconInfo },
} as const;

export function ToastViewport() {
  const toasts = useToasts();
  return (
    <RadixToast.Provider swipeDirection="right" duration={Infinity}>
      {toasts.map((t) => {
        const m = KIND_META[t.kind];
        return (
          <RadixToast.Root
            key={t.id}
            duration={Infinity /* lifetime owned by lib/toast */}
            onOpenChange={(open) => {
              if (!open) dismiss(t.id);
            }}
            className="enter-up pointer-events-auto flex items-start gap-2.5 rounded-md border border-border bg-raised px-3.5 py-3 shadow-[var(--shadow-overlay)]"
          >
            <span aria-hidden className="mt-0.5 shrink-0" style={{ color: m.color }}>
              <m.Icon />
            </span>
            <div className="min-w-0 flex-1">
              <RadixToast.Title className="text-[13px] font-medium text-ink">
                {t.title}
              </RadixToast.Title>
              {t.detail && (
                <RadixToast.Description className="mt-0.5 break-words text-xs text-muted">
                  {t.detail}
                </RadixToast.Description>
              )}
              {t.action && (
                <button
                  onClick={() => {
                    t.action?.onClick();
                    dismiss(t.id);
                  }}
                  className="mt-1.5 text-xs font-medium text-accent hover:underline"
                >
                  {t.action.label}
                </button>
              )}
            </div>
            <RadixToast.Close
              aria-label="Dismiss"
              className="shrink-0 rounded p-0.5 text-faint hover:bg-overlay hover:text-ink"
            >
              <IconClose size={13} />
            </RadixToast.Close>
          </RadixToast.Root>
        );
      })}
      <RadixToast.Viewport className="fixed bottom-4 right-4 z-[60] flex w-80 flex-col gap-2 outline-none" />
    </RadixToast.Provider>
  );
}
