import { useEffect, useState } from 'react';
import SyntaxHighlighter from 'react-syntax-highlighter/dist/esm/prism-light';

import { codeTheme } from '@/components/ui/ai/code-theme';
import { useGrammar } from '@/components/ui/ai/code-language';
import { cn, t } from '@/lib/utils';
import { ChevronDownIcon, Cog, Lock, X, Loader2 } from 'lucide-react';
import type { ToolChip } from '@lib/chat-types';
import { fetchToolCall, type ToolCallField, type ToolCallRecord } from '@lib/api';

/**
 * A tool call that did not succeed: failed (it errored) or rejected (a person
 * denied it). Both are shown in red, the way the CRM chat does. These are the
 * statuses the server actually sends on a finished tool.
 */
export function toolStatusIsError(status?: string | null): boolean {
  return status === 'failed' || status === 'rejected';
}

/** Compact human duration: 42ms, 1.2s, 12s. */
function formatDuration(ms?: number | null): string {
  if (ms == null || ms < 0) return '';
  if (ms < 1000) return `${ms}ms`;
  const s = ms / 1000;
  return `${s < 10 ? s.toFixed(1) : Math.round(s)}s`;
}

/**
 * A single tool row: an icon, the friendly name, a lock when it required
 * approval, and the execution time. Muted `text-sm text-muted-foreground` to
 * match the reasoning trigger. This is a LEAF — it carries no vertical margin of
 * its own, so the timeline container's single gap is the only source of spacing
 * between it and its neighbours (reasoning / text / other tools). Visibility is
 * decided by the caller (the `showTools` flag), never here.
 */
export function ToolRow({ tool, lang }: { tool: ToolChip; lang?: Record<string, string> }) {
  const isError = toolStatusIsError(tool.status);
  const isRunning = tool.status === 'running';
  const dur = formatDuration(tool.duration_ms);
  const label = tool.friendly_name || tool.name;

  // A finished call that has been written down can be opened. A running one
  // cannot: there is no result yet, and half of one is a panel that has to
  // explain itself.
  const canOpen = Boolean(tool.id) && !isRunning;
  const [open, setOpen] = useState(false);
  const [record, setRecord] = useState<ToolCallRecord | null>(null);
  const [failure, setFailure] = useState<string | null>(null);

  useEffect(() => {
    if (!open || record || !tool.id) return;
    let alive = true;
    // Asked for when it is opened, and remembered afterwards: opening the same
    // row twice is not two requests.
    fetchToolCall(tool.id)
      .then((answer) => alive && setRecord(answer))
      .catch((err) => alive && setFailure(String(err?.message ?? err)));
    return () => {
      alive = false;
    };
  }, [open, record, tool.id]);

  const row = (
    // The state is IN THE DOM, not only in the icon. A spinner is a picture: a
    // test can see that one exists but cannot ask how many calls are still
    // claiming to work, which is the question that matters after a reload. The
    // delegation chip learned this the same way, and the same defect (a call
    // left running for ever) went unseen for weeks because nothing could count
    // it.
    <div
      data-tool-state={tool.status}
      className="flex items-center gap-1.5 text-sm text-muted-foreground"
    >
      {isRunning ? (
        <Loader2 className="size-3.5 shrink-0 animate-spin opacity-70" />
      ) : isError ? (
        <X className="size-3.5 shrink-0 text-red-400" />
      ) : (
        <Cog className="size-3.5 shrink-0 opacity-50" />
      )}
      <span className={cn('truncate', isError && 'text-red-400')}>{label}</span>
      {tool.requested_approval && (
        <Lock
          className="size-3.5 shrink-0 opacity-50"
          aria-label={t('tool_requires_approval', lang, 'Required approval')}
        />
      )}
      {canOpen && (
        <ChevronDownIcon
          className={cn(
            'size-3.5 shrink-0 text-muted-foreground/60 transition-transform',
            open ? 'rotate-180' : 'rotate-0'
          )}
        />
      )}
      {dur && <span className="tabular-nums opacity-45">· {dur}</span>}
    </div>
  );

  if (!canOpen) return row;
  return (
    <div className="flex flex-col gap-1.5">
      <button
        type="button"
        onClick={() => setOpen((was) => !was)}
        aria-expanded={open}
        className="cursor-pointer text-left"
      >
        {row}
      </button>
      {open && (
        <ToolCallDetail record={record} failure={failure} lang={lang} />
      )}
    </div>
  );
}

/**
 * What one call was sent, and what it answered.
 *
 * Formatted rather than dumped. A tool's arguments and its result are JSON, and
 * JSON printed as one escaped line is unreadable: a command's output arrives as
 * "line one\nline two" and has to be read as two lines. So a string is shown as
 * text and everything else as indented JSON, which is the difference between a
 * panel somebody reads and one they scroll past.
 */
/**
 * A field on the panel: what the server sent, plus the one thing only the panel
 * knows, which is that a line is the error rather than part of the answer.
 */
type PanelField = ToolCallField & { tone?: 'error' };

/** Whether this call is one somebody is reading to find out what went wrong. */
function failed(record: ToolCallRecord): boolean {
  return toolStatusIsError(record.status);
}

function ToolCallDetail({
  record,
  failure,
  lang,
}: {
  record: ToolCallRecord | null;
  failure: string | null;
  lang?: Record<string, string>;
}) {
  if (failure) {
    return (
      <p className="text-xs text-muted-foreground">
        {failure === 'not_permitted'
          ? t('tool_detail_not_permitted', lang, 'You do not have permission to see what this tool did.')
          : t('tool_detail_unavailable', lang, 'This could not be loaded.')}
      </p>
    );
  }
  if (!record) {
    return <p className="text-xs text-muted-foreground">{t('loading', lang, 'Loading...')}</p>;
  }
  return (
    // ONE panel, not one per half. Two boxes stacked with a gap read as two
    // things that happened; what somebody opened is a single call, and its two
    // halves belong in one surface with a hairline between them.
    //
    // Full width, with no rule down its left. The indent belongs to reasoning,
    // where it marks an aside inside the answer; this is the panel itself, and
    // a border plus a margin around a surface that already has an edge is a
    // frame around a frame.
    <div className="fx-machine overflow-hidden rounded-lg bg-zinc-900 text-[12px] leading-relaxed text-zinc-300 ring-1 ring-white/10">
      <ToolCallSection fields={record.where} />
      <ToolCallSection title={t('tool_sent', lang, 'Sent')} fields={record.sent} />
      {/* ONE section for what came back, titled by whether it worked. A call
          that failed had an ANSWERED and an ERROR, which is two headings over
          one event and reads as though something came back AND something went
          wrong. When it went wrong, what came back IS what went wrong. */}
      {failed(record) ? (
        <ToolCallSection
          title={t('tool_error', lang, 'Error')}
          fields={[
            ...(record.error ? [{ name: '', value: record.error, tone: 'error' as const }] : []),
            ...(record.answered ?? []),
          ]}
        />
      ) : record.answered?.length ? (
        <ToolCallSection title={t('tool_answered', lang, 'Answered')} fields={record.answered} />
      ) : (
        // A command can succeed and print nothing, and saying so is the
        // difference between a finished call and a panel somebody thinks is
        // broken.
        <div className="border-t border-white/15 px-3 py-2.5">
          <span className="text-zinc-500">{t('tool_answered_nothing', lang, 'Nothing came back.')}</span>
        </div>
      )}
    </div>
  );
}

/**
 * One part of a call: where it ran, what it was sent, or what it answered.
 *
 * Dark, because what is in it is machine output: a command's stdout, a query's
 * rows, the JSON a service answered. A terminal is dark everywhere for the same
 * reason, and here it is what separates what the assistant SAID from what a
 * tool PRINTED without having to read either.
 *
 * Dark is not the same as loud. One surface, a hairline between the parts, two
 * weights of grey and nothing else: a field's name recedes, its value comes
 * forward, and no syntax colouring competes with either.
 *
 * WHICH fields are here was decided by the tool and applied by the server. This
 * renders what it is given, and knows no tool by name.
 */
function ToolCallSection({ title, fields }: { title?: string; fields?: PanelField[] }) {
  if (!fields || fields.length === 0) return null;
  const named = fields.filter((field) => field.name).length;
  return (
    <div className="border-t border-white/15 px-3 py-2.5 first:border-t-0">
      {title && <div className="mb-1.5 text-[10px] uppercase tracking-[0.08em] text-zinc-500">{title}</div>}
      {/* Scrolls inside itself: a command can print a great deal, and a
          conversation is not the place to scroll past all of it. */}
      <div className="max-h-72 overflow-auto">
        <div className="flex flex-col gap-1.5">
          {fields.map((field) => (
            // Alone means nothing else here needs a name to be told apart from
            // it. An error line has no name of its own, so the output beside it
            // is still the one thing being read.
            <ToolCallEntry key={field.name} field={field} alone={named <= 1} />
          ))}
        </div>
      </div>
    </div>
  );
}

/**
 * One named value.
 *
 * A field a tool marked as the SUBJECT of its section (the command under Sent,
 * the output under Answered) is drawn without its name when it is the only one
 * there: "command" above a command, inside a section already headed SENT, is a
 * label for the thing a person is looking at. Its name comes back the moment it
 * has a sibling, because then the names are telling two things apart rather
 * than announcing one: stdout beside stderr needs both.
 *
 * Everything else is a name and a value, on one line when it fits on one line
 * and under its name when it does not, because a paragraph belongs at the left
 * margin rather than in a column indented past the name of the field it
 * arrived in.
 */
function ToolCallEntry({ field, alone }: { field: PanelField; alone: boolean }) {
  const { name, value, as, tone } = field;
  const subject =
    alone && (as === 'command' || as === 'text' || as === 'sql' || as === 'table' || as === 'body');
  const label = name && !subject ? <span className="shrink-0 text-zinc-500">{name}</span> : null;

  if (as === 'command' && typeof value === 'string') {
    return (
      <div>
        {label}
        <div className={cn(!subject && 'mt-0.5')}>
          <Painted text={value} language="bash" prompt />
        </div>
      </div>
    );
  }
  if (as === 'sql' && typeof value === 'string') {
    return (
      <div>
        {label}
        <div className={cn(!subject && 'mt-0.5')}>
          <Painted text={readableSQL(value)} language="sql" />
        </div>
      </div>
    );
  }
  if (as === 'body') {
    return (
      <div>
        {label}
        <div className={cn(!subject && 'mt-0.5')}>
          <Payload value={value} />
        </div>
      </div>
    );
  }
  if (as === 'table') {
    return (
      <div>
        {label}
        <div className={cn(!subject && 'mt-0.5')}>
          <ResultTable value={value} />
        </div>
      </div>
    );
  }
  if (as === 'text' && typeof value === 'string') {
    return (
      <div>
        {label}
        <div
          className={cn(
            'whitespace-pre-wrap break-words font-mono',
            !subject && 'mt-0.5',
            tone === 'error' ? 'text-rose-200/90' : 'text-zinc-200'
          )}
        >
          {value}
        </div>
      </div>
    );
  }

  const text = typeof value === 'string' ? value : null;
  const inline = text === null ? false : !text.includes('\n') && text.length < 90;
  if (text !== null && !inline) {
    return (
      <div>
        {label}
        <div
          className={cn(
            'mt-0.5 whitespace-pre-wrap break-words font-mono',
            tone === 'error' ? 'text-rose-200/90' : 'text-zinc-200'
          )}
        >
          {text}
        </div>
      </div>
    );
  }
  return (
    <div className="flex gap-2">
      {label}
      <span className={cn('min-w-0 break-words font-mono', tone === 'error' ? 'text-rose-200/90' : 'text-zinc-200')}>
        <Value value={value} />
      </span>
    </div>
  );
}

/**
 * A result set, drawn as a table.
 *
 * Rows and their column names arrive as one thing (the server puts them
 * together), because a list of names above a list of lists is the shape of the
 * data and not the shape of an answer.
 *
 * What makes a grid of two hundred rows readable is not decoration: a header
 * that stays put while you scroll, a line between rows so the eye keeps its
 * place across nine columns, numbers on the right where they can be compared,
 * and a cell that does not let one long email decide the width of the table.
 * The full value stays reachable on the cell itself, so nothing is lost by
 * trimming it.
 */
function ResultTable({ value }: { value: unknown }) {
  const set = value as { columns?: unknown; rows?: unknown } | null;
  const columns = Array.isArray(set?.columns) ? (set.columns as unknown[]) : [];
  const rows = Array.isArray(set?.rows) ? (set.rows as unknown[]) : [];
  if (rows.length === 0) return null;

  return (
    <div className="overflow-x-auto">
      <table className="w-max border-collapse font-mono text-[12px]">
        {columns.length > 0 && (
          <thead className="sticky top-0 bg-zinc-900">
            <tr>
              {columns.map((name, i) => (
                <th
                  key={i}
                  className="whitespace-nowrap border-b border-white/15 px-2 py-1 text-left font-normal"
                  style={{ color: 'var(--code-name)' }}
                >
                  {String(name)}
                </th>
              ))}
            </tr>
          </thead>
        )}
        <tbody>
          {rows.map((row, i) => (
            <tr key={i} className="border-t border-white/5 first:border-t-0">
              {(Array.isArray(row) ? row : [row]).map((cell, j) => (
                <Cell key={j} value={cell} />
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** How long a cell may be before one email decides the table's width. */
const CELL_LIMIT = 48;

function Cell({ value }: { value: unknown }) {
  if (value === null || value === undefined) {
    return (
      <td className="px-2 py-1 align-top text-zinc-600">
        <span className="italic">null</span>
      </td>
    );
  }
  if (typeof value === 'number' || typeof value === 'boolean') {
    // On the right, where numbers are compared down a column.
    return <td className="px-2 py-1 text-right align-top tabular-nums text-zinc-200">{String(value)}</td>;
  }
  const text = typeof value === 'object' ? safeJSON(value) : String(value);
  const long = text.length > CELL_LIMIT;
  return (
    <td className="max-w-[28rem] px-2 py-1 align-top text-zinc-200" title={long ? text : undefined}>
      <span className="block truncate">{text}</span>
    </td>
  );
}

/**
 * A statement over several lines, the way anybody would write it out.
 *
 * A model sends SQL as one line, and one line of nine columns and three joins
 * is not read, it is scrolled. Each major clause starts a line: that is the
 * whole of it, no re-formatting of what is inside a clause, because the point
 * is to see the SHAPE of the statement and anything cleverer starts changing
 * somebody's SQL for them.
 *
 * Quoted text is skipped, so a keyword inside a string is left where it is.
 */
const CLAUSES =
  /\b(select|from|where|group\s+by|order\s+by|having|limit|offset|union all|union|left\s+join|right\s+join|inner\s+join|outer\s+join|cross\s+join|join|values|set|returning)\b/gi;

function readableSQL(sql: string): string {
  if (sql.includes('\n')) return sql; // already written out

  // Quoted text is copied through untouched; everything else is scanned for
  // clauses. A keyword inside a string is part of somebody's data.
  let out = '';
  let outside = '';
  let quote: string | null = null;
  const scan = () => {
    out += outside.replace(CLAUSES, (clause) => '\n' + clause);
    outside = '';
  };
  for (const c of sql) {
    if (quote) {
      out += c;
      if (c === quote) quote = null;
      continue;
    }
    if (c === "'" || c === '"' || c === '`') {
      scan();
      out += c;
      quote = c;
      continue;
    }
    outside += c;
  }
  scan();
  return out.replace(/^\s*\n/, '').trim();
}

/**
 * A payload, laid out and painted as whatever it is.
 *
 * The service says what it sent (a content type), so this reads rather than
 * guesses; when it says nothing, the first character decides, which is the
 * same test every tool that has ever had to do this uses. JSON arrives on one
 * line and is laid out; XML is left as it came, because re-indenting a
 * document can change what it means.
 */
function Payload({ value }: { value: unknown }) {
  const carried = value as { content_type?: unknown; body?: unknown } | null;
  const type = typeof carried?.content_type === 'string' ? carried.content_type.toLowerCase() : '';
  const raw = carried && 'body' in carried ? carried.body : value;
  const text = typeof raw === 'string' ? raw : safeJSON(raw);
  const trimmed = text.trim();

  const looksJSON = trimmed.startsWith('{') || trimmed.startsWith('[');
  const looksXML = trimmed.startsWith('<');
  if (type.includes('json') || (!type && looksJSON)) {
    return <Painted text={laidOut(text)} language="json" />;
  }
  if (type.includes('xml') || type.includes('html') || (!type && looksXML)) {
    return <Painted text={text} language="markup" />;
  }
  return <div className="whitespace-pre-wrap break-words font-mono text-zinc-200">{text}</div>;
}

/** JSON on one line, laid out. Anything that will not parse is left alone. */
function laidOut(text: string): string {
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    return text;
  }
}

/**
 * A command or a statement, painted by the highlighter this chat already has.
 *
 * Not a tokeniser of our own. There was one here for a day: forty lines of
 * regex that knew about flags and quotes and stopped at a heredoc, which is
 * most of a shell grammar written badly. The chat ships prism-light with the
 * grammars fetched on demand (code-language.ts), the same eight colours as
 * every code block in every answer, and it knows what a heredoc is.
 *
 * The grammar arrives when it arrives; until then this renders as plain text,
 * readable from the first frame.
 */
function Painted({ text, language, prompt }: { text: string; language: string; prompt?: boolean }) {
  const grammar = useGrammar(language);
  return (
    <div className="flex gap-1.5 font-mono">
      {prompt && <span className="select-none text-zinc-600">$</span>}
      <SyntaxHighlighter
        className="min-w-0 flex-1"
        codeTagProps={{ className: 'font-mono' }}
        customStyle={{
          margin: 0,
          padding: 0,
          background: 'none',
          fontSize: 'inherit',
          lineHeight: 'inherit',
          overflowX: 'auto',
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-word',
        }}
        language={grammar}
        style={codeTheme}
      >
        {text}
      </SyntaxHighlighter>
    </div>
  );
}

/**
 * A value that is not a command and not output: a number, a flag, a list, an
 * object a tool answered with. Drawn as what it is rather than as JSON, and
 * only a shape too nested to lay out flat falls back to indented JSON.
 */
function Value({ value, depth = 0 }: { value: unknown; depth?: number }): React.ReactElement | null {
  if (value === null || value === undefined || value === '') return null;
  if (typeof value === 'string') {
    return <span className="whitespace-pre-wrap break-words">{value}</span>;
  }
  if (typeof value !== 'object') return <span>{String(value)}</span>;
  if (depth > 2) {
    return <span className="whitespace-pre-wrap break-words">{safeJSON(value)}</span>;
  }
  if (Array.isArray(value)) {
    return (
      <span className="flex flex-col gap-1">
        {value.map((item, i) => (
          <Value key={i} value={item} depth={depth + 1} />
        ))}
      </span>
    );
  }
  const fields = Object.entries(value as Record<string, unknown>).filter(
    ([, v]) => v !== null && v !== undefined && v !== ''
  );
  if (fields.length === 0) return null;
  return (
    <span className="flex flex-col gap-1">
      {fields.map(([name, v]) => (
        <span key={name} className="flex gap-2">
          <span className="shrink-0 text-zinc-500">{name}</span>
          <span className="min-w-0 break-words">
            <Value value={v} depth={depth + 1} />
          </span>
        </span>
      ))}
    </span>
  );
}

function safeJSON(value: unknown): string {
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

/**
 * Legacy grouped renderer — a list of ToolRows. Kept for any caller that still
 * hands over a tool array; its gap matches the timeline rhythm so a group never
 * reads tighter than the surrounding reasoning/text. The flat timeline in
 * App.tsx renders individual ToolRows directly instead of grouping.
 */
export function ToolTimeline({ tools, className, lang }: { tools: ToolChip[]; className?: string; lang?: Record<string, string> }) {
  if (!tools || tools.length === 0) return null;
  return (
    <div className={cn('flex flex-col gap-3 select-none', className)}>
      {tools.map((tool, i) => (
        <ToolRow key={i} tool={tool} lang={lang} />
      ))}
    </div>
  );
}
