import { Tooltip as RadixTooltip } from "radix-ui";
import type { ReactNode } from "react";

/** Mount once in App. */
export function TooltipProvider(props: { children: ReactNode }) {
  return (
    <RadixTooltip.Provider delayDuration={300} skipDelayDuration={200}>
      {props.children}
    </RadixTooltip.Provider>
  );
}

export function Tip(props: {
  content: ReactNode;
  children: ReactNode;
  side?: "top" | "bottom" | "left" | "right";
  mono?: boolean;
}) {
  if (props.content == null) return <>{props.children}</>;
  return (
    <RadixTooltip.Root>
      <RadixTooltip.Trigger asChild>{props.children}</RadixTooltip.Trigger>
      <RadixTooltip.Portal>
        <RadixTooltip.Content
          side={props.side ?? "top"}
          sideOffset={6}
          collisionPadding={8}
          className={`enter-up z-50 max-w-72 rounded-md border border-border bg-raised px-2.5 py-1.5 text-xs text-ink shadow-[var(--shadow-overlay)] ${
            props.mono ? "font-mono tabular-nums" : ""
          }`}
        >
          {props.content}
        </RadixTooltip.Content>
      </RadixTooltip.Portal>
    </RadixTooltip.Root>
  );
}
