import * as React from "react";
import { cn } from "@/lib/utils";

/**
 * The multi-line counterpart to Input, in the same visual language. No
 * variant existed before the review queue's decision reason field, which
 * is the first place in this app that asks for more than one line of
 * free text from a person.
 */
export const Textarea = React.forwardRef<HTMLTextAreaElement, React.TextareaHTMLAttributes<HTMLTextAreaElement>>(
  ({ className, rows = 3, ...props }, ref) => {
    return (
      <textarea
        ref={ref}
        rows={rows}
        className={cn(
          "flex w-full rounded-md border border-ink-300 bg-white px-3.5 py-2.5 text-sm text-ink-950",
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
Textarea.displayName = "Textarea";
