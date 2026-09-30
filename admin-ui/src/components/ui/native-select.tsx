import * as React from 'react'
import { ChevronDown } from 'lucide-react'
import { cn } from '@/lib/utils'

/**
 * A styled native select, for forms choosing one of a few things.
 *
 * Native, because a status picker needs no popper machinery. The browser's
 * own arrow ignores the control's padding and sits on the border, so it is
 * hidden and ours is drawn inside the padding, where the text already lives.
 *
 * The OPTIONS carry their own colours, and that is not belt and braces.
 *
 * The list a native select drops down is not part of the page, and who draws it
 * differs: macOS hands it to the system, which paints a menu and ignores the
 * page entirely, while Windows draws it here out of the element's own colours.
 * The control is `bg-transparent`, so those colours were a transparent
 * background under text the dark theme makes light, and the list came up as
 * pale text on a pale ground. It was invisible on a Mac because a Mac never
 * read it.
 *
 * What is set is what macOS already shows: `--popover` is white in the light
 * theme and dark in the dark one, the same as every other menu. So a platform
 * that ignores this is unchanged, and a platform that honours it lands on the
 * colours it was drawing anyway. It is stated rather than left implied.
 */
function NativeSelect({ className, children, ...props }: React.ComponentProps<'select'>) {
  return (
    <div className="relative">
      <select
        data-slot="native-select"
        className={cn(
          'h-9 w-full min-w-0 appearance-none rounded-md border border-input bg-transparent pl-3 pr-9 text-sm shadow-xs outline-none',
          '[&>option]:bg-popover [&>option]:text-popover-foreground',
          'focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50',
          'disabled:cursor-not-allowed disabled:opacity-50',
          className,
        )}
        {...props}
      >
        {children}
      </select>
      <ChevronDown className="pointer-events-none absolute right-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
    </div>
  )
}

export { NativeSelect }
