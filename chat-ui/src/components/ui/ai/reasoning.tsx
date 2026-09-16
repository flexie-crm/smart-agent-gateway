import { useControllableState } from '@radix-ui/react-use-controllable-state';
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '@/components/ui/collapsible';
import { cn } from '@/lib/utils';
import { ChevronDownIcon, Lightbulb } from 'lucide-react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { normalizeMarkdown } from '@/lib/content-normalizer';
import type { ComponentProps } from 'react';
import { createContext, memo, useContext, useEffect, useMemo, useState } from 'react';

/** Lightweight markdown components sized for reasoning content (small, muted) */
function createReasoningComponents() {
  const base = 'text-xs text-muted-foreground';
  return {
    p: ({ children }: any) => <p className={cn(base, 'mb-2 mt-0 leading-relaxed')}>{children}</p>,
    strong: ({ children }: any) => <strong className="font-semibold text-muted-foreground text-xs">{children}</strong>,
    em: ({ children }: any) => <em className={cn(base, 'italic')}>{children}</em>,
    h1: ({ children }: any) => <div className="font-medium text-muted-foreground text-xs mt-2 mb-2">{children}</div>,
    h2: ({ children }: any) => <div className="font-medium text-muted-foreground text-xs mt-2 mb-2">{children}</div>,
    h3: ({ children }: any) => <div className="font-medium text-muted-foreground text-xs mt-2 mb-2">{children}</div>,
    h4: ({ children }: any) => <div className="font-medium text-muted-foreground text-xs mt-1 mb-2">{children}</div>,
    h5: ({ children }: any) => <div className="font-medium text-muted-foreground text-xs mt-1 mb-2">{children}</div>,
    h6: ({ children }: any) => <div className="font-medium text-muted-foreground text-xs mt-1 mb-2">{children}</div>,
    ul: ({ children }: any) => <ul className={cn(base, 'ml-4 list-disc space-y-0.5 leading-relaxed')}>{children}</ul>,
    ol: ({ children }: any) => <ol className={cn(base, 'ml-4 list-decimal space-y-0.5 leading-relaxed')}>{children}</ol>,
    li: ({ children }: any) => <li className={cn(base, 'pl-0.5 leading-relaxed')}>{children}</li>,
    a: ({ children, ...props }: any) => <a className="text-xs text-muted-foreground underline" {...props}>{children}</a>,
    code: ({ children, className }: any) => {
      // Inline code only — no fancy code blocks in reasoning
      if (className?.includes('language-')) {
        return <pre className="text-xs text-muted-foreground bg-muted/40 rounded p-2 my-1 overflow-x-auto"><code>{children}</code></pre>;
      }
      return <code className="text-xs text-muted-foreground bg-muted/40 rounded px-1 py-0.5">{children}</code>;
    },
    pre: ({ children }: any) => <pre className="text-xs text-muted-foreground bg-muted/40 rounded p-2 my-1 overflow-x-auto">{children}</pre>,
    blockquote: ({ children }: any) => <blockquote className={cn(base, 'border-l-2 border-muted-foreground/30 pl-2 my-1 italic')}>{children}</blockquote>,
    hr: () => <hr className="my-2 border-t border-border/40" />,
    table: ({ children }: any) => <div className="my-1 overflow-x-auto"><table className="text-xs text-muted-foreground border-collapse">{children}</table></div>,
    th: ({ children }: any) => <th className="text-xs font-medium text-muted-foreground border border-border/40 px-2 py-0.5">{children}</th>,
    td: ({ children }: any) => <td className="text-xs text-muted-foreground border border-border/40 px-2 py-0.5">{children}</td>,
  };
}
type ReasoningContextValue = {
  isStreaming: boolean;
  hasReasoning: boolean;
  isOpen: boolean;
  setIsOpen: (open: boolean) => void;
  duration: number;
  reasoningSource?: string;
};
const ReasoningContext = createContext<ReasoningContextValue | null>(null);
const useReasoning = () => {
  const context = useContext(ReasoningContext);
  if (!context) {
    throw new Error('Reasoning components must be used within Reasoning');
  }
  return context;
};
export type ReasoningProps = ComponentProps<typeof Collapsible> & {
  isStreaming?: boolean;
  hasReasoning?: boolean;
  open?: boolean;
  defaultOpen?: boolean;
  onOpenChange?: (open: boolean) => void;
  duration?: number;
  reasoningSource?: string;
};
export const Reasoning = memo(
  ({
    className,
    isStreaming = false,
    hasReasoning = false,
    open,
    defaultOpen = false,
    onOpenChange,
    duration: durationProp,
    reasoningSource,
    children,
    ...props
  }: ReasoningProps) => {
    const [isOpen, setIsOpen] = useControllableState({
      prop: open,
      defaultProp: defaultOpen,
      onChange: onOpenChange,
    });
    const [duration, setDuration] = useControllableState({
      prop: durationProp,
      defaultProp: 0,
    });
    const [startTime, setStartTime] = useState<number | null>(null);
    // Track duration when streaming starts and ends
    useEffect(() => {
      if (isStreaming) {
        if (startTime === null) {
          setStartTime(Date.now());
        }
      } else if (startTime !== null) {
        setDuration(Math.round((Date.now() - startTime) / 1000));
        setStartTime(null);
      }
    }, [isStreaming, startTime, setDuration]);
    // Stays collapsed by default — user controls expand/collapse manually
    const handleOpenChange = (newOpen: boolean) => {
      setIsOpen(newOpen);
    };
    return (
      <ReasoningContext.Provider
        value={{ isStreaming, hasReasoning, isOpen, setIsOpen, duration, reasoningSource }}
      >
        <Collapsible
          className={cn('not-prose', className)}
          onOpenChange={handleOpenChange}
          open={isOpen}
          {...props}
        >
          {children}
        </Collapsible>
      </ReasoningContext.Provider>
    );
  }
);
export type ReasoningTriggerProps = ComponentProps<
  typeof CollapsibleTrigger
> & {
  title?: string;
};
export const ReasoningTrigger = memo(
  ({
    className,
    title = 'Reasoning',
    children,
    ...props
  }: ReasoningTriggerProps) => {
    const { isStreaming, hasReasoning, isOpen, duration, reasoningSource } = useReasoning();
    const sourceLabel = reasoningSource ? ` · ${reasoningSource}` : '';
    return (
      <CollapsibleTrigger
        className={cn(
          'flex items-center gap-1.5 text-muted-foreground text-sm transition-colors select-none',
          hasReasoning ? 'hover:text-muted-foreground cursor-pointer' : 'cursor-default',
          className
        )}
        // Open while it is still being written, which is when somebody most
        // wants to read it. It used to be disabled during streaming, so the one
        // moment the reasoning was live was the one moment it could not be
        // opened, and by the time it could the model had moved on.
        disabled={!hasReasoning}
        {...props}
      >
        {children ?? (
          <>
            {/* No spinner and no "Reasoning…" here. The activity line below the
                row says what the agent is doing, once, and this is the thing it
                is doing it to: a block you can open and read. Two spinners
                saying the same word in one row is how the indicator came to be
                two elements in the first place. */}
            <Lightbulb className="size-3.5 shrink-0 opacity-50" />
            <span>
              {duration > 0
                ? `Reasoned for ${duration}s${sourceLabel}`
                : `Thought process${sourceLabel}`}
            </span>
            {hasReasoning && (
              <ChevronDownIcon
                style={{height: '20px', width: '20px', marginTop: '4px'}}
                className={cn(
                  'size-3 text-muted-foreground/60 transition-transform',
                  isOpen ? 'rotate-180' : 'rotate-0'
                )}
              />
            )}
          </>
        )}
      </CollapsibleTrigger>
    );
  }
);
export type ReasoningContentProps = ComponentProps<
  typeof CollapsibleContent
> & {
  children: string;
};
export const ReasoningContent = memo(
  ({ className, children, style, ...props }: ReasoningContentProps) => {
    const components = useMemo(() => createReasoningComponents(), []);
    return (
      <CollapsibleContent
        style={{marginLeft: '7px', marginTop: '10px', ...style}}
        className={cn(
          'text-xs text-muted-foreground leading-relaxed',
          'data-[state=closed]:fade-out-0 data-[state=closed]:slide-out-to-top-2 data-[state=open]:slide-in-from-top-2 outline-none data-[state=closed]:animate-out data-[state=open]:animate-in',
          className
        )}
        {...props}
      >
        <div className="border-l-2 border-muted pl-3">
          <div className="reasoning-content">
            <ReactMarkdown components={components} remarkPlugins={[remarkGfm]}>
              {typeof children === 'string' ? normalizeMarkdown(children) : children}
            </ReactMarkdown>
          </div>
        </div>
      </CollapsibleContent>
    );
  }
);
Reasoning.displayName = 'Reasoning';
ReasoningTrigger.displayName = 'ReasoningTrigger';
ReasoningContent.displayName = 'ReasoningContent';
