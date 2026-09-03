import * as React from "react";
import { cn } from "@/lib/utils";

export const Input = React.forwardRef<HTMLInputElement, React.InputHTMLAttributes<HTMLInputElement>>(
  ({ className, type = "text", ...props }, ref) => {
    return (
      <input
        ref={ref}
        type={type}
        className={cn(
          "flex h-11 w-full rounded-md border border-ink-300 bg-white px-3.5 text-sm text-ink-950",
          "placeholder:text-ink-400",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-amber-600 focus-visible:border-amber-600",
          "disabled:cursor-not-allowed disabled:opacity-50",
          className,
        )}
        {...props}
      />
    );
  },
);
Input.displayName = "Input";
