import * as React from "react";
import { cn } from "@/lib/utils";

type BadgeVariant = "default" | "outline" | "success" | "warning" | "danger";

const variantClasses: Record<BadgeVariant, string> = {
  default: "bg-ink-100 text-ink-700 border-ink-200",
  outline: "bg-transparent text-ink-600 border-ink-300",
  success: "bg-signal-500/10 text-signal-600 border-signal-500/30",
  warning: "bg-amber-500/10 text-amber-600 border-amber-500/30",
  danger: "bg-red-500/10 text-red-600 border-red-500/30",
};

export interface BadgeProps extends React.HTMLAttributes<HTMLSpanElement> {
  variant?: BadgeVariant;
}

export function Badge({ className, variant = "default", ...props }: BadgeProps) {
  return (
    <span
      className={cn(
        "inline-flex items-center rounded-full border px-2.5 py-0.5 text-xs font-medium",
        variantClasses[variant],
        className,
      )}
      {...props}
    />
  );
}
