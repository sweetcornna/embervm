// Route-driven tab bar (Radix Tabs supplies the keyboard/ARIA behavior; the
// workspace variant drives value from the URL so tabs deep-link).

import { Tabs as RadixTabs } from "radix-ui";
import type { ReactNode } from "react";

export interface TabDef {
  value: string;
  label: ReactNode;
  icon?: ReactNode;
  disabled?: boolean;
}

export function TabBar(props: {
  tabs: TabDef[];
  value: string;
  onChange: (value: string) => void;
  right?: ReactNode;
}) {
  return (
    <RadixTabs.Root value={props.value} onValueChange={props.onChange}>
      <RadixTabs.List
        className="flex items-center gap-0.5 border-b border-hairline px-2"
        aria-label="Workspace sections"
      >
        {props.tabs.map((t) => (
          <RadixTabs.Trigger
            key={t.value}
            value={t.value}
            disabled={t.disabled}
            className="relative -mb-px inline-flex items-center gap-1.5 rounded-t-md border-b-2 border-transparent px-3 py-2 text-[13px] font-medium text-muted transition-colors hover:text-ink disabled:cursor-not-allowed disabled:opacity-40 data-[state=active]:border-accent data-[state=active]:text-ink"
          >
            {t.icon}
            {t.label}
          </RadixTabs.Trigger>
        ))}
        {props.right && <div className="ml-auto pr-1">{props.right}</div>}
      </RadixTabs.List>
    </RadixTabs.Root>
  );
}
