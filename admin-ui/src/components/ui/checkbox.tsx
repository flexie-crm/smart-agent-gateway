import * as React from "react"
import * as CheckboxPrimitive from "@radix-ui/react-checkbox"
import { CheckIcon } from "lucide-react"
import { cn } from "@/lib/utils"

/**
 * A checkbox with its label (and an optional hint) as ONE component, aligned
 * once, here. The label owns the text metrics (text-sm, leading-5), so the box
 * has a real 20px line to center against; a call site composing its own label
 * around a bare box gets the alignment subtly wrong in a different way every
 * time, which is exactly what this exists to end.
 */
function CheckboxField({
  label,
  hint,
  checked,
  onChange,
  disabled,
  className,
}: {
  label: React.ReactNode
  hint?: React.ReactNode
  checked: boolean
  onChange: (checked: boolean) => void
  disabled?: boolean
  className?: string
}) {
  return (
    <label
      className={cn(
        "flex items-start gap-2.5 text-sm leading-5",
        disabled ? "opacity-60" : "cursor-pointer",
        className
      )}
    >
      {/* The box centers on the label's first line: its slot is one line high
          (h-5 matches leading-5), so a wrapping label keeps the box level with
          the first line rather than drifting to the middle of the block. */}
      <span className="flex h-5 shrink-0 items-center">
        <Checkbox
          checked={checked}
          disabled={disabled}
          onCheckedChange={(v) => onChange(v === true)}
        />
      </span>
      <span className="min-w-0">
        <span className="block">{label}</span>
        {hint && (
          <span className="mt-0.5 block text-xs leading-4 text-muted-foreground">{hint}</span>
        )}
      </span>
    </label>
  )
}

function Checkbox({
  className,
  ...props
}: React.ComponentProps<typeof CheckboxPrimitive.Root>) {
  return (
    <CheckboxPrimitive.Root
      data-slot="checkbox"
      className={cn(
        "peer flex h-4 w-4 shrink-0 cursor-pointer items-center justify-center rounded border border-border " +
          "transition-colors ring-offset-background focus-visible:outline-none focus-visible:ring-2 " +
          "focus-visible:ring-ring focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50 " +
          // Checked is a filled box, or checking one looks like nothing
          // happened: the indicator alone is a light check on a light field.
          "data-[state=checked]:border-primary data-[state=checked]:bg-primary data-[state=checked]:text-primary-foreground",
        className
      )}
      {...props}
    >
      <CheckboxPrimitive.Indicator className="flex items-center justify-center text-primary-foreground">
        <CheckIcon className="h-3 w-3" />
      </CheckboxPrimitive.Indicator>
    </CheckboxPrimitive.Root>
  )
}

export { Checkbox, CheckboxField }
