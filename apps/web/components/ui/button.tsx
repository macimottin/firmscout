import * as React from "react";
import { cn } from "@/lib/utils";

type ButtonVariant = "primary" | "secondary" | "ghost";
type ButtonSize = "default" | "sm" | "lg";

const variantClasses: Record<ButtonVariant, string> = {
  primary:
    "bg-ink-950 text-white hover:bg-ink-800 border border-transparent",
  secondary:
    "bg-white text-ink-900 border border-ink-300 hover:bg-ink-100",
  ghost: "bg-transparent text-ink-900 hover:bg-ink-100 border border-transparent",
};

const sizeClasses: Record<ButtonSize, string> = {
  default: "h-10 px-4 text-sm",
  sm: "h-8 px-3 text-sm",
  lg: "h-12 px-6 text-base",
};

export interface ButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
}

export const Button = React.forwardRef<HTMLButtonElement, ButtonProps>(
  ({ className, variant = "primary", size = "default", ...props }, ref) => {
    return (
      <button
        ref={ref}
        className={cn(
          "inline-flex items-center justify-center gap-2 rounded-md font-medium transition-colors",
          "focus-visible:outline-none disabled:pointer-events-none disabled:opacity-50",
          variantClasses[variant],
          sizeClasses[size],
          className,
        )}
        {...props}
      />
    );
  },
);
Button.displayName = "Button";
