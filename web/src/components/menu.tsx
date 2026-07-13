// DropdownMenu on Radix — correct focus wrap, typeahead, escape layering.
// Style stays in the token system; danger items get the status red.

import { DropdownMenu as RadixMenu } from "radix-ui";
import type { ReactNode } from "react";

export function Menu(props: { trigger: ReactNode; children: ReactNode; align?: "start" | "end" }) {
  return (
    <RadixMenu.Root>
      <RadixMenu.Trigger asChild>{props.trigger}</RadixMenu.Trigger>
      <RadixMenu.Portal>
        <RadixMenu.Content
          align={props.align ?? "end"}
          sideOffset={6}
          collisionPadding={8}
          className="enter-down z-50 min-w-44 rounded-md border border-border bg-raised p-1 shadow-[var(--shadow-overlay)]"
        >
          {props.children}
        </RadixMenu.Content>
      </RadixMenu.Portal>
    </RadixMenu.Root>
  );
}

export function MenuItem(props: {
  children: ReactNode;
  onSelect: () => void;
  danger?: boolean;
  disabled?: boolean;
  icon?: ReactNode;
}) {
  return (
    <RadixMenu.Item
      disabled={props.disabled}
      onSelect={props.onSelect}
      className={`flex cursor-default select-none items-center gap-2 rounded px-2.5 py-1.5 text-[13px] outline-none data-[disabled]:cursor-not-allowed data-[disabled]:opacity-40 ${
        props.danger
          ? "text-danger data-[highlighted]:bg-danger/10"
          : "text-ink data-[highlighted]:bg-overlay"
      }`}
    >
      {props.icon && <span className="text-muted">{props.icon}</span>}
      {props.children}
    </RadixMenu.Item>
  );
}

export function MenuSeparator() {
  return <RadixMenu.Separator className="mx-1 my-1 h-px bg-hairline" />;
}
