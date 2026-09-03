import * as React from "react";
import { cn } from "@/lib/utils";

type AlertVariant = "info" | "warning" | "danger";

const variantClasses: Record<AlertVariant, string> = {
  info: "bg-ink-100 border-ink-300 text-ink-800",
  warning: "bg-amber-500/10 border-amber-500/40 text-amber-700",
  danger: "bg-red-500/10 border-red-500/40 text-red-700",
};

export interface AlertProps extends React.HTMLAttributes<HTMLDivElement> {
  variant?: AlertVariant;
}

export function Alert({ className, variant = "info", role = "status", ...props }: AlertProps) {
  return (
    <div
      role={role}
      className={cn("rounded-md border px-4 py-3 text-sm", variantClasses[variant], className)}
      {...props}
    />
  );
}

export function AlertTitle({ className, ...props }: React.HTMLAttributes<HTMLParagraphElement>) {
  return <p className={cn("mb-1 font-semibold", className)} {...props} />;
}

export function AlertDescription({ className, ...props }: React.HTMLAttributes<HTMLParagraphElement>) {
  return <p className={cn("text-sm leading-relaxed", className)} {...props} />;
}
