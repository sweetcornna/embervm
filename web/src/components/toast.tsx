// Toast viewport — renders the lib/toast queue via Radix Toast (aria-live,
// swipe dismiss, focus hotkey). Mount once in App.

import { Toast as RadixToast } from "radix-ui";
import { useI18n } from "../lib/i18n";
import { dismiss, useToasts } from "../lib/toast";
import { IconCheck, IconClose, IconInfo, IconWarn } from "./icons";

const KIND_META = {
  success: { color: "var(--color-ok)", Icon: IconCheck },
  error: { color: "var(--color-danger)", Icon: IconWarn },
  info: { color: "var(--color-cold)", Icon: IconInfo },
} as const;

export function ToastViewport() {
  const { t } = useI18n();
  const toasts = useToasts();
  return (
    <RadixToast.Provider swipeDirection="right" duration={Infinity}>
      {toasts.map((item) => {
        const m = KIND_META[item.kind];
        return (
          <RadixToast.Root
            key={item.id}
            duration={Infinity /* lifetime owned by lib/toast */}
            onOpenChange={(open) => {
              if (!open) dismiss(item.id);
            }}
            className="enter-up pointer-events-auto flex items-start gap-2.5 rounded-md border border-border bg-raised px-3.5 py-3 shadow-[var(--shadow-overlay)]"
          >
            <span aria-hidden className="mt-0.5 shrink-0" style={{ color: m.color }}>
              <m.Icon />
            </span>
            <div className="min-w-0 flex-1">
              <RadixToast.Title className="text-[13px] font-medium text-ink">
                {item.title}
              </RadixToast.Title>
              {item.detail && (
                <RadixToast.Description className="mt-0.5 break-words text-xs text-muted">
                  {item.detail}
                </RadixToast.Description>
              )}
              {item.action && (
                <button
                  onClick={() => {
                    item.action?.onClick();
                    dismiss(item.id);
                  }}
                  className="mt-1.5 text-xs font-medium text-accent hover:underline"
                >
                  {item.action.label}
                </button>
              )}
            </div>
            <RadixToast.Close
              aria-label={t("Dismiss", "关闭")}
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
