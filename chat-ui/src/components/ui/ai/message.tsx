import { cn } from '@/lib/utils';
import type { UIMessage } from '@lib/chat-types';
import type { HTMLAttributes } from 'react';
export type MessageProps = HTMLAttributes<HTMLDivElement> & {
  from: UIMessage['role'];
  /** Said, and on its way into a turn that is still running. */
  waiting?: boolean;
};
export const Message = ({ className, from, waiting, ...props }: MessageProps) => (
  <div
    className={cn(
      'group flex w-full items-end justify-end gap-2',
      from === 'user' ? 'is-user whitespace-pre-wrap break-words leading-relaxed' : 'is-assistant flex-row-reverse justify-end',
      // Said, and on its way into a turn that is still running. Dimmed for the
      // moment it takes to be handed over, so that waiting looks like waiting
      // rather than like nothing having happened.
      waiting && 'opacity-60',
      className
    )}
    {...props}
  />
);
export type MessageContentProps = HTMLAttributes<HTMLDivElement>;
export const MessageContent = ({
  children,
  className,
  ...props
}: MessageContentProps) => (
  <div
    className={cn(
      'flex flex-col gap-2 overflow-hidden rounded-lg text-foreground text-[0.95rem]',
      // What the person said, in tokens rather than a fixed slate. It was
      // `bg-slate-100 text-slate-900`, which is a white card with near-black
      // text: correct in daylight and a lamp pointed at the reader every few
      // lines at night.
      'group-[.is-user]:px-4 group-[.is-user]:py-3 group-[.is-user]:bg-said group-[.is-user]:text-said-foreground group-[.is-user]:border-said-border',
      className
    )}
    {...props}
  >
    <div>{children}</div>
  </div>
);
