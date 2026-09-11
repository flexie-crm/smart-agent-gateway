import { cn } from '@/lib/utils';
import { normalizeMarkdown } from '@/lib/content-normalizer';
import type { ComponentProps, HTMLAttributes } from 'react';
import { isValidElement, lazy, memo, Suspense, useMemo } from 'react';
import ReactMarkdown, { type Options } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { Loader2 } from 'lucide-react'
import hardenReactMarkdown from 'harden-react-markdown';

import { ChartBlock } from '@components/ui/ai/chart-block';

// Lazy-load heavy components to split them into separate chunks. A chart is NOT
// one of them any more: it is drawn as SVG, in this bundle, so there is nothing
// to fetch and nothing to preload on idle to hide the fetching.
const LazyCodeBlock = lazy(() => import('@components/ui/ai/code-block').then(m => ({ default: m.CodeBlock })));
const LazyCodeBlockCopyButton = lazy(() => import('@components/ui/ai/code-block').then(m => ({ default: m.CodeBlockCopyButton })));

const ChunkLoadingFallback = () => (
  <div className="flex items-center gap-2 p-4 text-sm text-muted-foreground">
    <Loader2 className="size-4 animate-spin" />
    <span>Loading...</span>
  </div>
);

/**
 * Markdown normalization for streaming content.
 * Delegates to the content-normalizer pipeline.
 * Re-exported for backward compatibility (used by reasoning.tsx).
 */
export const parseIncompleteMarkdown = normalizeMarkdown;

// Create a hardened version of ReactMarkdown
const HardenedMarkdown = hardenReactMarkdown(ReactMarkdown);

function extractFirstJsonObject(str: string) {
  let inStr = false, esc = false, depth = 0, start = -1;

  for (let i = 0; i < str.length; i++) {
    const ch = str[i];

    if (inStr) {
      if (esc) { esc = false; continue; }
      if (ch === "\\") { esc = true; continue; }
      if (ch === '"') inStr = false;
      continue;
    }

    if (ch === '"') { inStr = true; continue; }

    if (ch === "{") {
      if (depth === 0) start = i;
      depth++;
      continue;
    }

    if (ch === "}") {
      if (depth > 0) {
        depth--;
        if (depth === 0 && start !== -1) {
          return str.slice(start, i + 1);
        }
      }
    }
  }

  return null; // none / incomplete
}


export type ResponseProps = Omit<HTMLAttributes<HTMLDivElement>, 'lang'> & {
  options?: Options;
  children: Options['children'];
  allowedImagePrefixes?: ComponentProps<
    ReturnType<typeof hardenReactMarkdown>
  >['allowedImagePrefixes'];
  allowedLinkPrefixes?: ComponentProps<
    ReturnType<typeof hardenReactMarkdown>
  >['allowedLinkPrefixes'];
  defaultOrigin?: ComponentProps<
    ReturnType<typeof hardenReactMarkdown>
  >['defaultOrigin'];
  parseIncompleteMarkdown?: boolean;
  useMarkdown?: boolean;
  sendMessage?: (text: string) => void;
  lang?: Record<string, string>;
  isStreaming?: boolean;
};

function createComponents(sendMessage?: (text: string) => void, lang?: Record<string, string>): Options['components'] {
  return {
  ol: ({ node, children, className, ...props }) => (
    <ol
      className={cn(
        'ml-5 list-decimal space-y-1 marker:text-muted-foreground/70 text-[0.95rem] leading-relaxed',
        className
      )}
      {...props}
    >
      {children}
    </ol>
  ),
  li: ({ node, children, className, ...props }) => (
    <li
      className={cn(
        'pl-1 text-foreground/90 [&>ul]:mt-1 [&>ol]:mt-1',
        className
      )}
      {...props}
    >
      {children}
    </li>
  ),
  ul: ({ node, children, className, ...props }) => (
    <ul
      className={cn(
        'ml-5 list-disc space-y-1 marker:text-muted-foreground/70 text-[0.95rem] leading-relaxed',
        className
      )}
      {...props}
    >
      {children}
    </ul>
  ),
  hr: ({ node, className, ...props }) => (
    <hr
      className={cn('my-5 border-t border-border/50', className)}
      {...props}
    />
  ),

  strong: ({ node, children, className, ...props }) => (
    <strong
      className={cn('font-semibold text-foreground', className)}
      {...props}
    >
      {children}
    </strong>
  ),

  a: ({ node, children, className, ...props }) => (
    <a
      className={cn(
        'underline-offset-2 hover:underline font-medium text-[0.95rem]',
        className
      )}
      rel="noreferrer"
      target="_blank"
      {...props}
    >
      {children}
    </a>
  ),

  h1: ({ node, children, className, ...props }) => (
    <h1
      className={cn(
        'mt-6 mb-3 text-2xl font-semibold tracking-tight text-foreground',
        className
      )}
      {...props}
    >
      {children}
    </h1>
  ),

  h2: ({ node, children, className, ...props }) => (
    <h2
      className={cn(
        'mt-6 mb-2 text-xl font-semibold tracking-tight text-foreground',
        className
      )}
      {...props}
    >
      {children}
    </h2>
  ),

  h3: ({ node, children, className, ...props }) => (
    <h3
      className={cn(
        'mt-5 mb-2 text-lg font-semibold tracking-tight text-foreground',
        className
      )}
      {...props}
    >
      {children}
    </h3>
  ),

  h4: ({ node, children, className, ...props }) => (
    <h4
      className={cn(
        'mt-4 mb-2 text-base font-semibold tracking-tight text-foreground',
        className
      )}
      {...props}
    >
      {children}
    </h4>
  ),

  h5: ({ node, children, className, ...props }) => (
    <h5
      className={cn(
        'mt-3 mb-2 text-[0.95rem] font-semibold text-foreground/90',
        className
      )}
      {...props}
    >
      {children}
    </h5>
  ),

  h6: ({ node, children, className, ...props }) => (
    <h6
      className={cn(
        'mt-3 mb-2 text-[0.9rem] font-semibold text-foreground/80',
        className
      )}
      {...props}
    >
      {children}
    </h6>
  ),

  table: ({ node, children, className, ...props }) => (
    <div className="my-4 overflow-x-auto rounded-md border border-border/60 bg-muted/20">
      <table
        className={cn(
          'w-full border-collapse text-sm text-foreground/90',
          className
        )}
        {...props}
      >
        {children}
      </table>
    </div>
  ),

  thead: ({ node, children, className, ...props }) => (
    <thead
      className={cn('bg-muted/40 text-foreground font-medium', className)}
      {...props}
    >
      {children}
    </thead>
  ),

  tbody: ({ node, children, className, ...props }) => (
    <tbody
      className={cn('divide-y divide-border/50', className)}
      {...props}
    >
      {children}
    </tbody>
  ),

  tr: ({ node, children, className, ...props }) => (
    <tr
      className={cn('hover:bg-muted/30 transition-colors', className)}
      {...props}
    >
      {children}
    </tr>
  ),

  th: ({ node, children, className, ...props }) => (
    <th
      className={cn(
        'px-4 py-2 text-left font-semibold text-[0.9rem] text-foreground/90',
        className
      )}
      {...props}
    >
      {children}
    </th>
  ),

  td: ({ node, children, className, ...props }) => (
    <td
      className={cn('px-4 py-2 align-top text-[0.9rem] text-foreground/85', className)}
      {...props}
    >
      {children}
    </td>
  ),

  blockquote: ({ node, children, className, ...props }) => (
    <blockquote
      className={cn(
        'my-4 border-l-4 border-border/60 pl-4 text-muted-foreground italic bg-muted/10 rounded-sm',
        className
      )}
      {...props}
    >
      {children}
    </blockquote>
  ),
  code: ({ node, className, ...props }) => {
    const inline = node?.position?.start.line === node?.position?.end.line;
    if (!inline) {
      return <code className={className} {...props} />;
    }
    return (
      <code
        className={cn(
          'rounded bg-muted px-1.5 py-0.5 font-mono text-sm',
          className
        )}
        {...props}
      />
    );
  },
  pre: ({ node, className, children }) => {
    // Extract text content safely (handles React elements, strings, and arrays)
    const extractCode = (c: unknown): string => {
      if (!c) return '';
      if (typeof c === 'string') return c;
      if (Array.isArray(c)) return c.map((x) => extractCode(x)).join('');
      if (
        typeof c === 'object' &&
        c !== null &&
        'props' in c &&
        (c as any).props &&
        typeof (c as any).props.children === 'string'
      ) {
        return (c as any).props.children;
      }
      return '';
    };

    const codeText = extractCode(children).trim();

    const firstChild =
      Array.isArray((node as any)?.children) && (node as any).children.length > 0
        ? (node as any).children[0]
        : null;

    const classNameValue: string =
      (firstChild &&
        typeof firstChild === 'object' &&
        'properties' in firstChild &&
        Array.isArray((firstChild as any).properties?.className) &&
        (firstChild as any).properties.className[0]) ||
      '';

    const isChartBlock =
      (typeof classNameValue === 'string' && classNameValue.startsWith('language-chart')) ||
      (codeText.trim().startsWith('{') &&
        codeText.includes('"type"') &&
        codeText.includes('"data"') &&
        codeText.includes('"datasets"'));

    // --- Handle chart blocks ---
    if (isChartBlock) {
      const chartJson = extractFirstJsonObject(codeText) || codeText;

      // Try parsing the JSON to determine if this chart's data is complete.
      // During streaming, the normalizer auto-closes code fences with partial
      // JSON, so the parse will fail — we show a placeholder per-chart.
      // Once the closing ``` arrives with complete JSON, it parses and we render.
      let chartDataComplete = false;
      try {
        const parsed = JSON.parse(chartJson);
        chartDataComplete = !!(parsed && parsed.type && parsed.data);
      } catch {
        // Incomplete JSON — chart is still streaming
      }

      const chartFallback = (
        <div className="my-4 rounded-md border bg-muted/40 p-4 h-72 flex items-center justify-center">
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />
            <span>Chart is being created...</span>
          </div>
        </div>
      );

      if (!chartDataComplete) {
        return chartFallback;
      }

      // No Suspense: nothing is fetched. The chart is drawn from the numbers
      // that just arrived, in this bundle.
      return (
        <div className="my-4 rounded-md border bg-muted/40 p-4">
          <ChartBlock json={chartJson} />
        </div>
      );
    }

    if (typeof classNameValue === 'string' && (classNameValue.startsWith('language-') || classNameValue.startsWith('programming-'))) {
      let language = classNameValue.replace(/^(language-|programming-)/, '');
      let code = '';

      if (
        isValidElement(children) &&
        children.props &&
        typeof (children.props as any).children === 'string'
      ) {
        code = (children.props as any).children;
      } else if (typeof children === 'string') {
        code = children;
      }

      return (
        <Suspense fallback={<ChunkLoadingFallback />}>
          <LazyCodeBlock
            className={cn('my-4 h-auto', className)}
            code={code}
            language={language}
          >
            <LazyCodeBlockCopyButton
              onCopy={() => console.log('Copied code to clipboard')}
              onError={() => console.error('Failed to copy code to clipboard')}
            />
          </LazyCodeBlock>
        </Suspense>
      );
    }

    // --- Default: render a plain preformatted code block ---
    return (
      <pre
        className={cn(
          'my-4 overflow-x-auto rounded-md border border-border/60 bg-muted/50 p-4',
          className
        )}
        style={{
          fontFamily: '"Fira Code", ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Courier New", monospace',
          whiteSpace: 'pre',
          wordWrap: 'normal',
          overflowWrap: 'normal',
          fontSize: '0.85rem',
          lineHeight: 1.6,
          tabSize: 4,
        }}
      >
        {children}
      </pre>
    );
  },
  };
}

export const Response = memo(
  ({
    className,
    options,
    children,
    allowedImagePrefixes,
    allowedLinkPrefixes,
    defaultOrigin,
    parseIncompleteMarkdown: shouldParseIncompleteMarkdown = true,
    useMarkdown = true,
    sendMessage,
    lang,
    isStreaming,
    ...props
  }: ResponseProps) => {
    const components = useMemo(() => createComponents(sendMessage, lang), [sendMessage, lang]);
    // Parse the children to remove incomplete markdown tokens if enabled
    const parsedChildren =
      typeof children === 'string' && shouldParseIncompleteMarkdown
        ? normalizeMarkdown(children)
        : children;

    // Use this without markdown for the user, and give AI all formatting instead
    if (!useMarkdown) {
      return (
        <div
          className={cn(
            'size-full whitespace-pre-wrap break-words [&>p:not(:first-child)]:mt-[0.3rem] [&>p]:mb-[0.3rem] [&>p]:[margin-block:calc(.25rem*1)]',
            className
          )}
          {...props}
        >
          {children}
        </div>
      );
    }

    return (
      <div
        className={cn(
          'size-full [&>p:not(:first-child)]:mt-[0.3rem] [&>p]:mb-[0.3rem] [&>p]:[margin-block:calc(.25rem*1)]',
          className
        )}
        {...props}
      >
        <HardenedMarkdown
          allowedImagePrefixes={allowedImagePrefixes ?? ['*']}
          allowedLinkPrefixes={allowedLinkPrefixes ?? ['*']}
          components={components}
          defaultOrigin={defaultOrigin}
          rehypePlugins={[]}
          remarkPlugins={[remarkGfm]}
          {...options}
        >
          {parsedChildren}
        </HardenedMarkdown>
      </div>
    );
  },
  (prevProps, nextProps) =>
    prevProps.children === nextProps.children &&
    prevProps.sendMessage === nextProps.sendMessage &&
    prevProps.lang === nextProps.lang &&
    prevProps.isStreaming === nextProps.isStreaming
);

Response.displayName = 'Response';
