import * as React from "react"

import { cn } from "@/lib/utils"

// An optional leading icon. The bare <input> is still what comes back when
// there is none, so no existing call site changes shape.
function Input({
  className,
  type,
  icon,
  ...props
}: React.ComponentProps<"input"> & { icon?: React.ReactNode }) {
  const field = (
    <input
      type={type}
      data-slot="input"
      className={cn(
        "h-9 w-full min-w-0 rounded-md border border-input bg-transparent px-3 py-1 text-base shadow-xs transition-[color,box-shadow] outline-none selection:bg-primary selection:text-primary-foreground file:inline-flex file:h-7 file:border-0 file:bg-transparent file:text-sm file:font-medium file:text-foreground placeholder:text-muted-foreground disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50 md:text-sm dark:bg-input/30",
        "focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50",
        "aria-invalid:border-destructive aria-invalid:ring-destructive/20 dark:aria-invalid:ring-destructive/40",
        icon && "pl-7",
        className
      )}
      {...props}
    />
  )

  if (!icon) return field

  return (
    // data-slot on the wrapper, like every other part in this layer: a screen
    // whose field must fill its column (the watch rail's find box) has a hook
    // to widen it without reaching for a structural selector.
    <span
      data-slot="input-wrapper"
      className="relative inline-flex items-center [&>svg]:pointer-events-none [&>svg]:absolute [&>svg]:left-2 [&>svg]:size-4 [&>svg]:text-muted-foreground"
    >
      {icon}
      {field}
    </span>
  )
}

export { Input }
