import { useState } from "react";
import { Check, Copy } from "lucide-react";
import { Button } from "@/components/ui/button";

/**
 * One value somebody has to paste somewhere else, exactly as it is.
 *
 * Read-only and monospaced on purpose. This is not a field: nothing here is
 * being configured, and an input somebody could edit would invite them to fix a
 * value that is only correct unchanged. `break-all` rather than `break-words`,
 * because a URL has no word boundaries to break at and must never be shown
 * truncated when the thing it is for is an exact match.
 */
export function CopyLine({ value, label }: { value: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="rounded-md border border-border">
      {label && (
        <div className="border-b border-border px-3 py-1.5 text-xs text-muted-foreground">
          {label}
        </div>
      )}
      <div className="flex items-center justify-between gap-2 px-3 py-2">
        <code className="break-all font-mono text-xs leading-relaxed">
          {value}
        </code>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="shrink-0"
          onClick={async () => {
            try {
              await navigator.clipboard.writeText(value);
              setCopied(true);
              window.setTimeout(() => setCopied(false), 2000);
            } catch {
              // A browser that refuses the clipboard leaves the value on screen
              // to select by hand, which is what it was always for.
            }
          }}
        >
          {copied ? <Check className="size-4" /> : <Copy className="size-4" />}
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
    </div>
  );
}
