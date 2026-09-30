import { memo } from 'react';
import { Loader2 } from 'lucide-react';
import { Message, MessageContent } from '@/components/ui/ai/message';
import { Response } from '@/components/ui/ai/response';
import { Reasoning, ReasoningContent, ReasoningTrigger } from '@/components/ui/ai/reasoning';
import { ToolRow } from '@/components/ui/ai/tool-timeline';
import { ConfirmBlock } from '@/components/ui/ai/confirm-block';
import { AttachmentImage, getFileTypeInfo, isImageFile } from '@/components/attachments';
import { isConfirmMessage, isAssistantMessage } from '@lib/chat-types';
import { assistantTimelineItems, rendersNothing } from '@lib/message-parts';
import type { ChatMessage, AgentActivity } from '@lib/chat-types';
import { activityLabel } from '@lib/activity-label';

// One message of a conversation, as the chat draws it. The chat's own column
// and an agent opened from its chip both draw through this, so an agent's work
// looks exactly like the chat that asked for it.

/**
 * Per-role visual overrides. Any React CSSProperties key is accepted and
 * passed verbatim to inline `style`. Common ones are `fontSize`, `color`,
 * `background`; but you can also pass `padding`, `border`, `borderRadius`,
 * `boxShadow`, `margin`, etc.
 *
 * Convenience: if you set `background` (or `backgroundColor`) without an
 * explicit `padding`, the assistant and reasoning roles auto-receive a
 * sensible inset + rounding so text doesn't sit flush against the bubble edge.
 * Set `padding` explicitly to opt out.
 */
export type RoleTheme = React.CSSProperties;
export interface ChatTheme {
  user?: RoleTheme;
  assistant?: RoleTheme;
  reasoning?: RoleTheme;
}

// ─── Memoized message item ──────────────────────────────────────────────────
// Extracted so completed messages (stable object references from the stream
// processor) skip rendering entirely on every streaming chunk / RAF tick.

interface MessageItemProps {
  message: ChatMessage;
  /** What the agent is doing. The streaming row shows it, so that what the
   *  agent is doing and what it has said are one element the virtual list
   *  measures together. It used to be a footer BELOW the list: the moment
   *  reasoning began, that footer unmounted while a reasoning block mounted
   *  inside the row, and the column changed height twice in two places for
   *  what a reader sees as one continuous state. */
  activity: AgentActivity;
  showReasoning: boolean;
  showTools: boolean;
  theme?: ChatTheme;
  /** Absent where nothing can be sent from, like an agent opened to read. */
  sendMessage?: (text: string) => void;
  /** Absent where nothing can be answered, and then a card is not drawn: a
   *  card with buttons that do nothing is worse than no card. */
  respondToConfirmation?: (token: string, action: 'approved' | 'rejected' | 'approved_all') => void;
  lang?: Record<string, string>;
}

/** Passes every key on `t` through to inline `style`. When a background is
 * set without an explicit `padding`, applies sensible inset + rounding —
 * Tailwind only pads the user role by default, so a custom background on
 * assistant/reasoning would otherwise leave text flush against the edge. */
function roleStyle(t: RoleTheme | undefined, paddingWhenBg?: string): React.CSSProperties | undefined {
  if (!t || Object.keys(t).length === 0) return undefined;
  const out: React.CSSProperties = { ...t };
  const hasBg = t.background !== undefined || t.backgroundColor !== undefined;
  if (paddingWhenBg && hasBg && out.padding === undefined) {
    out.padding = paddingWhenBg;
    if (out.borderRadius === undefined) out.borderRadius = '0.5rem';
  }
  return out;
}

/** One object, so a completed row's props never change identity. */
export const IDLE: AgentActivity = { kind: 'idle' };

/**
 * What the agent is doing, at the end of the row it is doing it in.
 *
 * Reasoning is deliberately absent when it is being SHOWN: the reasoning block
 * above says "Reasoning…" with its own spinner, and two spinners saying the
 * same thing is worse than one. When reasoning is withheld there is no block,
 * so this speaks for it.
 *
 * Text arriving needs no line at all. The text is the indicator.
 */
const ActivityLine = memo(({ activity, lang }: {
  activity: AgentActivity;
  lang?: Record<string, string>;
}) => {
  // The decision is activityLabel's (lib/activity-label.ts), where it can be
  // tested without building an application and watching a turn. This draws it.
  const label = activityLabel(activity, lang);
  if (label === null) return null;
  return (
    <div className="flex items-center gap-2 text-muted-foreground text-sm">
      <Loader2 className="size-4 animate-spin" />
      <span>{label}</span>
    </div>
  );
});
ActivityLine.displayName = 'ActivityLine';

export const MessageItem = memo(
  ({ message, activity, showReasoning, showTools, theme, sendMessage, respondToConfirmation, lang }: MessageItemProps) => {
    const isReasoningStreaming = activity.kind === 'reasoning';
    // user role already has Tailwind px-4 py-3 + rounded-lg, so no bg-padding default needed.
    const userStyle = roleStyle(theme?.user);
    const assistantStyle = roleStyle(theme?.assistant, '0.75rem 1rem');
    const reasoningStyle = roleStyle(theme?.reasoning, '0.4rem 0.75rem');
    if (isConfirmMessage(message)) {
      if (!respondToConfirmation) return null;
      // No wrapper. The row it sits in already spaces it like every other
      // message; the two divs that used to be here added a second gap under it,
      // so an answered approval was the one line in the transcript with double
      // the space beneath.
      return (
        <ConfirmBlock
          confirmation={message.confirmation}
          onRespond={respondToConfirmation}
          lang={lang}
        />
      );
    }

    const items = isAssistantMessage(message) ? assistantTimelineItems(message) : [];

    // An assistant turn with nothing in it renders NOTHING, streaming or not:
    // an empty child still costs the conversation's `space-y-3` on both sides
    // (see rendersNothing, which owns the rule and is tested).
    if (isAssistantMessage(message) && rendersNothing(message)) return null;

    return (
      // A message renders as ONE flat vertical timeline. Every item (reasoning
      // block, text block, individual tool row) is a direct child of a single
      // flex-col gap-3 container, so one gap (12px) is the sole source of spacing
      // between them. No nested tool group with its own gap, so N tool calls never
      // bunch tighter than the surrounding reasoning or text. User turns add
      // py-[18px] on top so the user/AI boundary reads as about 30px.
      <div className={message.role === 'user' ? 'flex flex-col gap-3 py-[18px]' : 'flex flex-col gap-3'}>
        {/* Error alert — shown when the AI vendor returns an error */}
        {isAssistantMessage(message) && message.error && (
          <div className="flex items-start gap-2.5 rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-800 dark:border-red-800/40 dark:bg-red-950/30 dark:text-red-300">
            <svg className="mt-0.5 size-4 shrink-0" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
              <path fillRule="evenodd" d="M18 10a8 8 0 11-16 0 8 8 0 0116 0zm-8-5a.75.75 0 01.75.75v4.5a.75.75 0 01-1.5 0v-4.5A.75.75 0 0110 5zm0 10a1 1 0 100-2 1 1 0 000 2z" clipRule="evenodd" />
            </svg>
            <span>{message.error}</span>
          </div>
        )}
        {/* File attachment cards — above the user message bubble (like ChatGPT) */}
        {message.role === 'user' && message.attachments && message.attachments.length > 0 && (
          <div className="flex flex-wrap gap-2 justify-end">
            {message.attachments.map((file) => {
              if (isImageFile(file)) {
                return (
                  <div key={file.id} className="w-16 h-16 rounded-xl overflow-hidden border border-border/40 shadow-sm">
                    {/* From the server, not from the local preview. The
                        preview is a blob URL that dies with the tab, so a
                        reloaded conversation showed a broken picture above the
                        question it belonged to. */}
                    <AttachmentImage id={file.id} alt={file.file_name} />
                  </div>
                );
              }
              const info = getFileTypeInfo(file);
              return (
                <div key={file.id} className="flex items-center gap-2 rounded-xl border border-border/40 bg-background/80 px-2.5 py-2 shadow-sm max-w-[200px]">
                  <div className="shrink-0 w-8 h-8 rounded-lg flex items-center justify-center" style={{ backgroundColor: info.bg }}>
                    <span className="text-[0.6rem] font-bold leading-none" style={{ color: info.color }}>{info.label}</span>
                  </div>
                  <div className="min-w-0">
                    <div className="text-xs font-medium text-foreground truncate leading-tight">{file.file_name}</div>
                    <div className="text-[0.6rem] text-muted-foreground leading-tight">{info.label}</div>
                  </div>
                </div>
              );
            })}
          </div>
        )}
        {/* User bubble (assistant text renders inside the flat timeline below). */}
        {message.role === 'user' && (
          <Message from="user" waiting={message.waiting}>
            <MessageContent style={userStyle}>
              <Response useMarkdown={false} sendMessage={sendMessage} lang={lang} isStreaming={false}>
                {message.attachments && message.attachments.length > 0
                  ? message.content.replace(/\n\n\[Attached files:[^\]]*\]$/, '')
                  : message.content}
              </Response>
            </MessageContent>
          </Message>
        )}
        {/* Assistant flat timeline: reasoning / text / tool items in order, each a
            direct sibling under the container's single gap. Live and reloaded
            messages produce the SAME list via assistantTimelineItems(), so a turn
            looks identical whether it just streamed or came back from history.
            showReasoning/showTools gate their items; text always renders. */}
        {isAssistantMessage(message) && items.map((part, i) => {
          if (part.kind === 'reasoning') {
            return showReasoning ? (
              <Reasoning
                key={i}
                isStreaming={!!message.isStreaming && isReasoningStreaming && i === items.length - 1}
                hasReasoning={true}
                defaultOpen={false}
                reasoningSource={part.model}
              >
                <ReasoningTrigger style={reasoningStyle} />
                <ReasoningContent style={reasoningStyle}>{part.text}</ReasoningContent>
              </Reasoning>
            ) : null;
          }
          if (part.kind === 'tool') {
            return showTools ? <ToolRow key={i} tool={part.tool} lang={lang} /> : null;
          }
          if (part.kind === 'agent') {
            // An agent's answer the Gateway delegated for: shown apart, so it
            // reads as the agent's work and not as the Gateway's own reply.
            return part.text ? (
              <div key={i} className="ml-10 border-l-2 border-border pl-3">
                <div className="mb-1 text-xs font-medium text-muted-foreground">
                  {part.agent ? `${part.agent}` : 'Agent'}
                </div>
                <div className="text-sm text-muted-foreground">
                  <Response useMarkdown sendMessage={sendMessage} lang={lang} isStreaming={!!message.isStreaming}>
                    {part.text}
                  </Response>
                </div>
              </div>
            ) : null;
          }
          return part.text ? (
            <Message key={i} from="assistant">
              <MessageContent style={assistantStyle}>
                <Response useMarkdown sendMessage={sendMessage} lang={lang} isStreaming={!!message.isStreaming}>
                  {part.text}
                </Response>
              </MessageContent>
            </Message>
          ) : null;
        })}
        {/* Last, because it is what happens NEXT. Inside the row rather than
            below the list, so the virtual list measures it with everything
            else and a change of state is one height change instead of two. */}
        {isAssistantMessage(message) && message.isStreaming && (
          <ActivityLine activity={activity} lang={lang} />
        )}
      </div>
    );
  },
  (prev, next) =>
    prev.message === next.message &&
    prev.activity === next.activity &&
    prev.showReasoning === next.showReasoning &&
    prev.showTools === next.showTools &&
    prev.theme === next.theme &&
    prev.sendMessage === next.sendMessage &&
    prev.respondToConfirmation === next.respondToConfirmation &&
    prev.lang === next.lang
);
MessageItem.displayName = 'MessageItem';
