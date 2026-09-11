import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';
import { CheckIcon, CopyIcon } from 'lucide-react';
import type { ComponentProps, HTMLAttributes, ReactNode } from 'react';
import { createContext, useContext, useEffect, useState } from 'react';
// The light build BY PATH, not `{ PrismLight } from 'react-syntax-highlighter'`.
// The package index also re-exports the full build, whose `refractor` import
// registers all 277 grammars; CommonJS defeats the tree-shaking that would
// otherwise drop it, and they come along.
import SyntaxHighlighter from 'react-syntax-highlighter/dist/esm/prism-light';
import { codeTheme } from './code-theme';
import { useGrammar } from './code-language';

type CodeBlockContextType = {
  code: string;
};
const CodeBlockContext = createContext<CodeBlockContextType>({
  code: '',
});
export type CodeBlockProps = HTMLAttributes<HTMLDivElement> & {
  code: string;
  language: string;
  showLineNumbers?: boolean;
  children?: ReactNode;
};
export const CodeBlock = ({
  code,
  language,
  showLineNumbers = false,
  className,
  children,
  ...props
}: CodeBlockProps) => {
  // The grammar arrives when it arrives; until then this is the empty string
  // and the code renders as plain text, readable from the first frame.
  const grammar = useGrammar(language);
  // And it is not applied until this block has SETTLED where it is.
  //
  // Highlighting is the most expensive thing in a conversation, and it used to
  // happen the instant a block appeared. That is the worst possible moment: a
  // block appears because it is being scrolled into view, so the tokeniser ran
  // inside the frame that was supposed to be moving the page. Measured on a
  // conversation with a code block in every fourth answer, scrolling dropped 30
  // frames out of 268; with the code blocks taken out, 3. It got worse rather
  // than better once rows were virtualised, because a row that scrolls away is
  // unmounted, and every trip back past it paid the whole cost again.
  //
  // Plain first, highlighted a beat later, and the beat is idle time rather than
  // a timer where there is such a thing. Scroll fast and the blocks you flew
  // past are never tokenised at all: they were unmounted before their turn came
  // up. Stop, and what you are looking at colours in. Nothing moves when it
  // does, because the text is the same text at the same size: only its colour
  // changes.
  const highlighting = useSettled();
  const reader = highlighting ? grammar : '';
  return (
  <CodeBlockContext.Provider value={{ code }}>
    <div
      className={cn(
        'relative w-full overflow-hidden rounded-md border bg-background text-foreground',
        className
      )}
      {...props}
    >
      <div className="relative">
        {/* ONE render. It used to be two, the whole block highlighted twice
            (oneLight, then oneDark) with CSS hiding the one you were not
            looking at, because the theme is a class on an ancestor this app
            does not own and inline token colours cannot follow it. They are CSS
            variables now, so `.dark` repaints this instead of replacing it. */}
        <SyntaxHighlighter
          className="overflow-hidden"
          codeTagProps={{
            className: 'font-mono text-sm',
          }}
          customStyle={{
            margin: 0,
            padding: '1rem',
            fontSize: '0.875rem',
            background: 'var(--muted)',
            color: 'var(--foreground)',
          }}
          language={reader}
          lineNumberStyle={{
            color: 'var(--muted-foreground)',
            paddingRight: '1rem',
            minWidth: '2.5rem',
          }}
          showLineNumbers={showLineNumbers}
          style={codeTheme}
        >
          {code}
        </SyntaxHighlighter>
        {children && (
          <div className="absolute top-2 right-2 flex items-center gap-2">
            {children}
          </div>
        )}
      </div>
    </div>
  </CodeBlockContext.Provider>
  );
};
export type CodeBlockCopyButtonProps = ComponentProps<typeof Button> & {
  onCopy?: () => void;
  onError?: (error: Error) => void;
  timeout?: number;
};
export const CodeBlockCopyButton = ({
  onCopy,
  onError,
  timeout = 2000,
  children,
  className,
  ...props
}: CodeBlockCopyButtonProps) => {
  const [isCopied, setIsCopied] = useState(false);
  const { code } = useContext(CodeBlockContext);
  const copyToClipboard = async () => {
    if (typeof window === 'undefined' || !navigator.clipboard.writeText) {
      onError?.(new Error('Clipboard API not available'));
      return;
    }
    try {
      await navigator.clipboard.writeText(code);
      setIsCopied(true);
      onCopy?.();
      setTimeout(() => setIsCopied(false), timeout);
    } catch (error) {
      onError?.(error as Error);
    }
  };
  const Icon = isCopied ? CheckIcon : CopyIcon;
  return (
    <Button
      className={cn('shrink-0', className)}
      onClick={copyToClipboard}
      size="icon"
      variant="ghost"
      {...props}
    >
      {children ?? <Icon size={14} />}
    </Button>
  );
};

/**
 * False on the frame a thing appears, true once the browser has a moment.
 *
 * `requestIdleCallback` where it exists, and a short timer where it does not,
 * which is Safari and therefore the desktop application: the point is only to be
 * off the frame that is scrolling, and either one of those achieves that.
 */
function useSettled(): boolean {
  const [settled, setSettled] = useState(false);
  useEffect(() => {
    const idle = (window as Window & {
      requestIdleCallback?: (cb: () => void, o?: { timeout: number }) => number;
      cancelIdleCallback?: (id: number) => void;
    });
    if (idle.requestIdleCallback) {
      const id = idle.requestIdleCallback(() => setSettled(true), { timeout: 400 });
      return () => idle.cancelIdleCallback?.(id);
    }
    const id = window.setTimeout(() => setSettled(true), 90);
    return () => window.clearTimeout(id);
  }, []);
  return settled;
}
