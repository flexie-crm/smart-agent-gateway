import * as React from "react"
import { Slot } from "@radix-ui/react-slot"
import { cva, type VariantProps } from "class-variance-authority"

import { cn } from "@/lib/utils"

const buttonVariants = cva(
  // transition only colour and the focus ring, never opacity: a button that is
  // disabled while a page loads (no vendor yet, say) would otherwise fade in
  // when it enables, which reads as a flicker. Hover and focus still animate.
  "inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-md text-sm font-medium transition-[color,background-color,border-color,box-shadow] disabled:pointer-events-none disabled:opacity-50 [&_svg]:pointer-events-none [&_svg:not([class*='size-'])]:size-4 shrink-0 [&_svg]:shrink-0 outline-none focus-visible:border-ring focus-visible:ring-ring/50 focus-visible:ring-[3px] aria-invalid:ring-destructive/20 dark:aria-invalid:ring-destructive/40 aria-invalid:border-destructive",
  {
    variants: {
      variant: {
        default: "bg-primary text-primary-foreground hover:bg-primary/90 cursor-pointer",
        destructive:
          "bg-destructive text-white hover:bg-destructive/90 focus-visible:ring-destructive/20 dark:focus-visible:ring-destructive/40 dark:bg-destructive/60 cursor-pointer",
        outline:
          "border bg-background shadow-xs hover:bg-accent hover:text-accent-foreground dark:bg-input/30 dark:border-input dark:hover:bg-input/50 cursor-pointer",
        secondary:
          "bg-secondary text-secondary-foreground hover:bg-secondary/80 cursor-pointer",
        ghost:
          // Black under the pointer is a daylight decision: at night it is a hole in
        // the page and the label vanishes into it. The same fix as the chat's.
        "text-accent-foreground hover:bg-black hover:text-white dark:hover:bg-accent dark:hover:text-accent-foreground cursor-pointer",
        link: "text-primary underline-offset-4 hover:underline cursor-pointer",
        dark: "bg-[#2f2f2f] text-white hover:bg-[#3a3a3a] border border-[#3a3a3a] cursor-pointer",
      },
      size: {
        // Vertical centering is the fixed height plus items-center, nothing
        // else: no vertical padding to drift out of balance, nothing for
        // devtools to show as lopsided.
        //
        // Horizontal: a leading icon gets 2px LESS left padding than the text
        // gets on the right (has-[>svg]:pl-*). The glyph is drawn inset
        // inside its svg box, so with equal box padding the pill reads
        // heavier on the left; shaving the box padding makes the VISIBLE
        // margins match. Icon-only sizes hold one centered glyph and stay
        // symmetric.
        default: "h-9 px-4 has-[>svg]:pl-3.5",
        sm: "h-8 rounded-md px-3 gap-1.5 has-[>svg]:pl-2.5",
        lg: "h-10 rounded-md px-6 has-[>svg]:pl-5.5",
        icon: "size-9",
        "icon-sm": "size-8",
        "icon-lg": "size-10",
      },
    },
    defaultVariants: {
      variant: "default",
      size: "default",
    },
  }
)

function Button({
  className,
  variant,
  size,
  asChild = false,
  ...props
}: React.ComponentProps<"button"> &
  VariantProps<typeof buttonVariants> & {
    asChild?: boolean
  }) {
  const Comp = asChild ? Slot : "button"

  return (
    <Comp
      data-slot="button"
      className={cn(buttonVariants({ variant, size, className }))}
      {...props}
    />
  )
}

export { Button, buttonVariants }
