// Route-driven tab bar (Radix Tabs supplies the keyboard/ARIA behavior; the
// workspace variant drives value from the URL so tabs deep-link).

import { Tabs as RadixTabs } from "radix-ui";
import type { ReactNode } from "react";
import { useI18n } from "../lib/i18n";

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
  const { t } = useI18n();
  return (
    <RadixTabs.Root value={props.value} onValueChange={props.onChange}>
      <RadixTabs.List
        className="flex items-center gap-0.5 border-b border-hairline px-2"
        aria-label={t("Workspace sections", "工作区分区")}
      >
        {props.tabs.map((tab) => (
          <RadixTabs.Trigger
            key={tab.value}
            value={tab.value}
            disabled={tab.disabled}
            className="relative -mb-px inline-flex items-center gap-1.5 rounded-t-md border-b-2 border-transparent px-3 py-2 text-[13px] font-medium text-muted transition-colors hover:text-ink disabled:cursor-not-allowed disabled:opacity-40 data-[state=active]:border-accent data-[state=active]:text-ink"
          >
            {tab.icon}
            {tab.label}
          </RadixTabs.Trigger>
        ))}
        {props.right && <div className="ml-auto pr-1">{props.right}</div>}
      </RadixTabs.List>
    </RadixTabs.Root>
  );
}
