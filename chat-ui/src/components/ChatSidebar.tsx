// components/ChatSidebar.tsx
// ─── Slide-over drawer listing the user's chat threads ──────────────────────
// Overlay (absolute within the chat panel) rather than a permanent column, so
// the existing compact/embedded layout is untouched. Pinned first, then recents;
// search, new-chat, per-row rename / pin / delete, and "Load more" pagination.

import { useEffect, useRef, useState } from 'react'
import {
  Plus,
  Search,
  Pin,
  PinOff,
  Trash2,
  Pencil,
  Check,
  Building2,
  FolderOpen,
  SlidersHorizontal,
} from 'lucide-react'
import { cn } from '@/lib/utils'
import { PERSONAL } from '@lib/api'
import {
  chooseFolder,
  chosenFolder,
  forgetFolder,
  insideTheApp,
  watchMachineLink,
} from '@/lib/machine-link'
import { t } from '@lib/utils'
import { ThemeSwitch } from './ThemeSwitch'
import { UpdateReady } from './UpdateReady'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import type { useChatList, ChatListItem } from '@lib/use-chat-list'

interface ChatSidebarProps {
  open: boolean
  // Docked = a permanent left column (full-screen / expanded mode), ChatGPT-style:
  // no backdrop, no slide transition, no close button. Otherwise it's an overlay
  // drawer toggled from the header.
  docked?: boolean
  onClose: () => void
  list: ReturnType<typeof useChatList>
  currentChatId: string | null
  onSelect: (id: string) => void
  onNewChat: () => void
  // Delete is delegated to the host so it can clear/re-select the current chat
  // when the deleted one is open.
  onDeleteChat: (id: string) => void
  lang?: Record<string, string>
  // Who is signed in, where, and the way out. It lives at the FOOT of the
  // sidebar rather than in a bar across the top of the conversation: those
  // three things are about the account, not about the chat, and a header that
  // existed only to carry them was a strip of chrome above every message.
  account?: {
    name: string
    email?: string
    workspaces?: { id: number; name: string }[]
    currentWorkspaceId?: number
    onSwitchWorkspace?: (id: number) => void
    onSignOut?: () => void
  }
}

function ChatRow({
  chat,
  active,
  lang,
  onSelect,
  onRename,
  onPin,
  onDelete,
}: {
  chat: ChatListItem
  active: boolean
  lang?: Record<string, string>
  onSelect: () => void
  onRename: (title: string) => void
  onPin: (pinned: boolean) => void
  /**
   * Undefined when this person may not delete a conversation (`chats:delete`).
   *
   * Absent rather than disabled: a greyed-out bin invites the question "why
   * not, and how do I get it", which the row cannot answer. A role either
   * grants this or it does not, and the answer lives in the console.
   */
  onDelete?: () => void
}) {
  const [editing, setEditing] = useState(false)
  // Inline delete confirmation: the delete icon flips the row's actions to a
  // Yes/No prompt instead of a native confirm() dialog. No -> restore actions.
  const [confirming, setConfirming] = useState(false)
  const [draft, setDraft] = useState(chat.title || '')
  // Re-entry guard: Enter calls commit() then setEditing(false), whose unmount
  // fires the Input's onBlur -> commit() again. Without this, one rename = two
  // POSTs. Escape sets it too, so the blur-after-escape does not commit.
  const committedRef = useRef(false)

  const title = chat.title && chat.title.trim() ? chat.title : t('chat_untitled', lang, 'New chat')

  const commit = () => {
    if (committedRef.current) return
    committedRef.current = true
    const next = draft.trim()
    setEditing(false)
    if (next && next !== chat.title) onRename(next)
  }

  if (editing) {
    return (
      <div className="flex items-center gap-1 px-2 py-1.5">
        <Input
          autoFocus
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') commit()
            if (e.key === 'Escape') { committedRef.current = true; setEditing(false) }
          }}
          onBlur={commit}
          className="h-7 text-sm"
        />
        <Button variant="ghost" size="icon-sm" onMouseDown={(e) => e.preventDefault()} onClick={commit}>
          <Check className="size-3.5" />
        </Button>
      </div>
    )
  }

  return (
    <div
      className={cn(
        // Fixed h-8 (not py-*) so the row height is identical whether it shows the
        // title only, the hover icons (taller), or the Yes/No confirm buttons —
        // no 2px jump between states. mb-0.5 keeps a gap between rows so an active
        // row next to a hovered one never reads as one glued block.
        'group mb-0.5 flex h-8 items-center gap-2 rounded-md px-2 text-sm',
        confirming ? 'bg-sidebar-accent' : cn('cursor-pointer', active ? 'bg-sidebar-accent' : 'hover:bg-sidebar-accent')
      )}
      onClick={confirming ? undefined : onSelect}
    >
      {/* Title uses the full width and only ellipsizes on genuine overflow. On
          hover the action group takes its (display:none until then) slot, so the
          title shrinks and shows the ellipsis then — never before. The row is
          width-bound by the ScrollArea fix, so this shrink stays in-bounds. */}
      <span className="min-w-0 flex-1 truncate">{title}</span>

      {confirming ? (
        /* Inline delete confirmation — always visible while confirming. Yes
           deletes; No restores the actions, no side effect. */
        <div className="flex shrink-0 items-center gap-1" onClick={(e) => e.stopPropagation()}>
          <span className="text-[0.7rem] text-muted-foreground">{t('chat_delete_q', lang, 'Delete?')}</span>
          <button
            type="button"
            className="cursor-pointer rounded px-1.5 py-0.5 text-[0.7rem] font-semibold text-red-600 hover:bg-red-50"
            onClick={() => { setConfirming(false); onDelete?.() }}
          >
            {t('chat_delete_yes', lang, 'Yes')}
          </button>
          <button
            type="button"
            className="cursor-pointer rounded px-1.5 py-0.5 text-[0.7rem] font-medium text-muted-foreground hover:bg-background hover:text-foreground"
            onClick={() => setConfirming(false)}
          >
            {t('chat_delete_no', lang, 'No')}
          </button>
        </div>
      ) : (
        /* Row actions — no reserved slot at rest (display:none), shown on hover;
           the title then ellipsizes to make room. */
        <div className="hidden shrink-0 items-center gap-0.5 group-hover:flex" onClick={(e) => e.stopPropagation()}>
          <button
            type="button"
            title={t('chat_rename', lang, 'Rename')}
            className="cursor-pointer rounded p-1 text-muted-foreground hover:bg-background hover:text-foreground"
            onClick={() => { setDraft(chat.title || ''); committedRef.current = false; setEditing(true) }}
          >
            <Pencil className="size-3.5" />
          </button>
          <button
            type="button"
            title={chat.is_pinned ? t('chat_unpin', lang, 'Unpin') : t('chat_pin', lang, 'Pin')}
            className="cursor-pointer rounded p-1 text-muted-foreground hover:bg-background hover:text-foreground"
            onClick={() => onPin(!chat.is_pinned)}
          >
            {chat.is_pinned ? <PinOff className="size-3.5" /> : <Pin className="size-3.5" />}
          </button>
          {onDelete && (
            <button
              type="button"
              title={t('chat_delete', lang, 'Delete')}
              className="cursor-pointer rounded p-1 text-muted-foreground hover:bg-background hover:text-red-600"
              onClick={() => setConfirming(true)}
            >
              <Trash2 className="size-3.5" />
            </button>
          )}
        </div>
      )}
    </div>
  )
}

/**
 * The folder on this computer the assistant starts in.
 *
 * A working directory, the way a terminal has one: commands run there and a
 * relative path is measured from it. Not a boundary — an absolute path reaches
 * what it names, because this runs as the person, on their own machine, and a
 * fence around a garden they own would be a pretence with the gate open beside
 * it. What actually bounds it is the account, the commands the tool permits,
 * and the approval they give.
 *
 * Shown only in the application, because only there is there a computer to
 * choose one on, and it sits with the workspace rather than in a settings
 * screen: it is the answer to "where is this thing working", which somebody
 * should be able to read without going to look for it.
 *
 * Removing it is next to it, and is how somebody stops the assistant reaching
 * their computer at all without signing out of anything.
 */
function WorkingFolder({ lang }: { lang?: Record<string, string> }) {
  // Three states, not two, and that is the whole of this.
  //
  // `folder` alone stood for both "we have not asked the application yet" and
  // "there is no folder", so the row opened by saying "Choose a working folder"
  // and then, a moment later, replaced it with the folder that had been chosen
  // all along. The height never moved, which is why measuring the box said it
  // was fine: what changed was the CLAIM. A row that states the opposite of the
  // truth and corrects itself reads as the page rebuilding under you.
  //
  // `asked` is what separates them. Until it is true the row is drawn and says
  // nothing, so it holds its place and asserts nothing it might have to take
  // back.
  const [folder, setFolder] = useState<string | null>(null);
  const [asked, setAsked] = useState(false);
  const [asking, setAsking] = useState(false);

  useEffect(() => {
    let alive = true;
    void chosenFolder().then((chosen) => {
      if (!alive) return;
      setFolder(chosen);
      setAsked(true);
    });
    return () => {
      alive = false;
    };
  }, []);

  if (!insideTheApp()) return null;

  const pick = async () => {
    setAsking(true);
    try {
      const chosen = await chooseFolder();
      if (chosen) setFolder(chosen);
    } catch {
      // They closed the dialog, or the system refused it. Nothing changed, and
      // there is nothing to say that the unchanged row does not already say.
    } finally {
      setAsking(false);
    }
  };

  const clear = async () => {
    await forgetFolder();
    setFolder(null);
  };

  return (
    <div className="shrink-0 border-b border-border px-2.5 py-2">
      <div className="flex h-8 items-center gap-2 px-2.5 text-sm text-muted-foreground">
        <FolderOpen className="size-4 shrink-0" />
        {!asked ? null : folder ? (
          <>
            <span className="truncate" title={folder}>
              {folder.split('/').pop() || folder}
            </span>
            <button
              type="button"
              onClick={() => void clear()}
              className="ml-auto shrink-0 cursor-pointer text-xs hover:text-foreground"
            >
              {t('remove', lang, 'Remove')}
            </button>
          </>
        ) : (
          <button
            type="button"
            disabled={asking}
            onClick={() => void pick()}
            className="cursor-pointer truncate text-left hover:text-foreground disabled:opacity-60"
          >
            {t('choose_folder', lang, 'Choose a working folder')}
          </button>
        )}
      </div>
    </div>
  );
}

/**
 * The workspace's mark, which also says whether this computer's network can be
 * reached.
 *
 * Green when the desktop application holds its route to the gateway, so a
 * database or a server that only this machine can see is reachable. Its
 * ordinary colour otherwise, which covers both "the route is down" and "there
 * is no route here at all" (a browser, or the personal edition where the
 * gateway is already on this machine). Nothing is added for a state that is not
 * a fault: an icon that is quiet means nothing to worry about.
 *
 * Colour and a soft glow, not a heavier line: it is the same mark either way,
 * lit or not, so it reads at a glance without the eye being asked to compare
 * two shapes.
 */
function WorkspaceMark() {
  const [up, setUp] = useState(false);
  useEffect(() => {
    let alive = true;
    let stop = () => {};
    void watchMachineLink((state) => {
      if (alive) setUp(state);
    }).then((off) => {
      if (alive) stop = off;
      else off();
    });
    return () => {
      alive = false;
      stop();
    };
  }, []);

  return (
    <Building2
      className={
        up
          ? 'size-4 shrink-0 text-emerald-500 drop-shadow-[0_0_4px_rgba(16,185,129,0.7)]'
          : 'size-4 shrink-0 text-muted-foreground'
      }
      aria-label={up ? 'Connected to this computer' : undefined}
    />
  );
}

export function ChatSidebar({ open, docked, onClose, list, currentChatId, onSelect, onNewChat, onDeleteChat, lang, account }: ChatSidebarProps) {
  const { chats, canDelete, loading, query, setQuery, hasMore, loadMore, renameChat, setPinned } = list

  const pinned = chats.filter((c) => c.is_pinned)
  const recents = chats.filter((c) => !c.is_pinned)

  const renderRow = (chat: ChatListItem) => (
    <ChatRow
      key={chat.id}
      chat={chat}
      active={chat.id === currentChatId}
      lang={lang}
      onSelect={() => onSelect(chat.id)}
      onRename={(title) => renameChat(chat.id, title)}
      onPin={(p) => setPinned(chat.id, p)}
      onDelete={canDelete ? () => onDeleteChat(chat.id) : undefined}
    />
  )

  return (
    <>
      {!docked && open && <div className="absolute inset-0 z-40 bg-black/30" onClick={onClose} aria-hidden="true" />}
      <div
        className={
          docked
            ? 'relative flex h-full w-[280px] shrink-0 flex-col border-r bg-sidebar'
            : cn(
                'absolute inset-y-0 left-0 z-50 flex w-[280px] max-w-[85%] flex-col border-r bg-sidebar shadow-xl transition-transform duration-200',
                open ? 'translate-x-0' : '-translate-x-full'
              )
        }
        role={docked ? undefined : 'dialog'}
        aria-label={t('chat_history', lang, 'Chat history')}
      >
        {/* No title, no close button. Both overlay and docked modes start straight
            at "New chat". The overlay closes by clicking the backdrop, a chat row,
            or New chat (all handled by the host). */}

        {/* The row is there before its content is.
            Both of these used to hang off `account`, which arrives with a
            request: the sidebar drew without them and then pushed New chat, the
            search and every conversation down when they landed. The company row
            reserves its height and fills in; the working folder never depended
            on the account in the first place, and asks the application itself. */}
        {account ? (
          <WorkspaceRow account={account} lang={lang} />
        ) : (
          <div className="h-14 shrink-0 border-b border-border" />
        )}
        <WorkingFolder lang={lang} />

        {/* New chat — no border/background at rest, only a subtle fill on hover. */}
        <div className="p-2.5">
          <Button variant="ghost" size="sm" className="w-full justify-start gap-2 font-normal hover:bg-sidebar-accent hover:text-foreground" onClick={onNewChat}>
            <Plus className="size-4" />
            {t('chat_new', lang, 'New chat')}
          </Button>
        </div>

        {/* Search — a flex row: items-center centers the icon, and the input fills
            the full height so its text/placeholder center natively on the same
            line. Border + focus ring live on the container (focus-within). This
            removes the absolute-icon / padding guesswork so both are truly centered. */}
        <div className="px-2.5 pb-2">
          <div className="flex h-8 items-center gap-2 rounded-md border border-input bg-transparent px-2.5 transition-[color,box-shadow] focus-within:border-ring focus-within:ring-[3px] focus-within:ring-ring/50">
            <Search className="pointer-events-none size-3.5 shrink-0 text-muted-foreground" />
            <input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder={t('chat_search', lang, 'Search chats')}
              className="h-full w-full min-w-0 bg-transparent text-sm text-foreground outline-none placeholder:text-muted-foreground"
            />
          </div>
        </div>

        {/* List */}
        <ScrollArea className="min-h-0 flex-1">
          <div className="px-2.5 pb-2">
            {loading && chats.length === 0 && (
              <div className="px-2 py-4 text-center text-xs text-muted-foreground">{t('loading', lang, 'Loading...')}</div>
            )}
            {!loading && chats.length === 0 && (
              <div className="px-2 py-4 text-center text-xs text-muted-foreground">
                {query ? t('chat_no_results', lang, 'No chats match your search') : t('chat_empty', lang, 'No chats yet')}
              </div>
            )}

            {pinned.length > 0 && (
              <>
                <div className="px-2 pt-1 pb-1 text-[0.65rem] font-semibold uppercase tracking-wide text-muted-foreground">
                  {t('chat_pinned', lang, 'Pinned')}
                </div>
                {pinned.map(renderRow)}
              </>
            )}
            {recents.length > 0 && (
              <>
                {pinned.length > 0 && (
                  <div className="px-2 pt-2 pb-1 text-[0.65rem] font-semibold uppercase tracking-wide text-muted-foreground">
                    {t('chat_recent', lang, 'Recent')}
                  </div>
                )}
                {recents.map(renderRow)}
              </>
            )}

            {/* Pagination — reveal the next window of threads on demand. */}
            {hasMore && (
              <div className="px-2.5 pt-2">
                <Button
                  variant="ghost"
                  size="sm"
                  className="w-full justify-center text-xs text-muted-foreground"
                  disabled={loading}
                  onClick={loadMore}
                >
                  {loading ? t('loading', lang, 'Loading...') : t('chat_load_more', lang, 'Load more')}
                </Button>
              </div>
            )}
          </div>
        </ScrollArea>

        {/* Above the footer, and OUTSIDE the personal branch below. It used to
            live inside it, which meant the enterprise edition could never show
            it: that edition carries no pages, so its chat is whatever a server
            serves, and a server serves the web build, where PERSONAL is false
            and this was compiled away. A shell that had quietly installed a new
            version had no way to say so.
            The gate was redundant as well as wrong. UpdateReady renders nothing
            unless it finds a desktop shell to listen to, so asking again at
            build time decided the wrong question: not "is there a shell" but
            "was this bundle made for the personal edition". */}
        <UpdateReady />
        {account && <AccountFooter account={account} lang={lang} />}
      </div>
    </>
  )
}

/**
 * Which workspace you are in, at the top of the list it scopes.
 *
 * The same shape as the console's (`admin-ui/AppShell`), on purpose: it is the
 * same decision in the same place, and two surfaces of one product should not
 * ask a person to learn it twice.
 *
 * A styled Select rather than a bare `<select>`, because a native one draws the
 * operating system's arrow, in the operating system's colour, which is black on
 * a dark sidebar and cannot be told otherwise. Somebody in ONE workspace has
 * nothing to choose, so they get the name and no control at all.
 */
function WorkspaceRow({
  account,
  lang,
}: {
  account: NonNullable<ChatSidebarProps['account']>
  lang?: Record<string, string>
}) {
  const workspaces = account.workspaces ?? []
  const current = workspaces.find((w) => w.id === account.currentWorkspaceId)
  if (!current) return null

  if (workspaces.length < 2 || !account.onSwitchWorkspace) {
    return (
      <>
        {/* h-14, which is the console's sidebar rule and the main header's.
            This was px-2 py-2 around an h-8 row, so 48 against 56, and the two
            products' first horizontal line disagreed by eight pixels: the sort
            of thing somebody sees before they can name it. */}
        <div className="flex h-14 shrink-0 items-center border-b border-border px-2.5">
          <div className="flex h-8 flex-1 items-center gap-2 px-2.5 text-sm text-muted-foreground">
            <WorkspaceMark />
            <span className="truncate">{current.name}</span>
          </div>
        </div>
      </>
    )
  }

  return (
    // px-2.5 here and px-2.5 on the trigger, which is 20px: where New chat's
    // icon, the search icon and every conversation begin, and where the
    // console's own column begins too. It was 18, and the console 20, so moving
    // between the two products slid the whole sidebar two pixels sideways: the
    // sort of thing somebody sees before they can name it.
    <div className="flex h-14 shrink-0 items-center border-b border-border px-2.5">
      <Select
        value={String(current.id)}
        onValueChange={(v) => account.onSwitchWorkspace?.(Number(v))}
      >
        <SelectTrigger
          size="sm"
          aria-label={t('workspace', lang, 'Workspace')}
          className="w-full border-transparent bg-transparent px-2.5 shadow-none hover:bg-sidebar-accent focus-visible:border-input"
        >
          <span className="flex min-w-0 items-center gap-2">
            <WorkspaceMark />
            <SelectValue />
          </span>
        </SelectTrigger>
        <SelectContent>
          {workspaces.map((w) => (
            <SelectItem key={w.id} value={String(w.id)}>
              {w.name}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  )
}

/**
 * The foot of the sidebar: who is signed in and the way out, or on a personal
 * installation the way to the console.
 *
 * There is nobody to sign out as when there is one person and no password, so
 * that row would be an exit from a building with no door. What belongs there
 * instead is the other half of the application: the console is where models,
 * tools and brains are set, and this is the only place the chat points at it.
 *
 * A plain `<button>` and not the shared `Button`: its ghost variant is
 * `hover:bg-black hover:text-white`, which on this row turns the whole thing
 * black under the pointer. A quiet control that tints its icon and lifts its
 * background a shade is what the console does, and what this row wants.
 */
function AccountFooter({
  account,
  lang,
}: {
  account: NonNullable<ChatSidebarProps['account']>
  lang?: Record<string, string>
}) {
  // On a personal installation this is the console link, and nothing else: no
  // identity, because there is only one, and no sign-out, because there is
  // nothing to sign out of.
  if (PERSONAL) {
    // The same class string as the console's sidebar footer.
    return (
      <>
      <div className="flex h-16 shrink-0 items-center gap-2 border-t border-border px-3">
        <a
          href="/"
          className="flex flex-1 items-center gap-2.5 rounded-md px-2 py-1.5 text-sm text-muted-foreground transition-colors hover:bg-sidebar-accent hover:text-foreground"
        >
          <SlidersHorizontal className="size-4 shrink-0" />
          {t('open_console', lang, 'Console')}
        </a>
        <ThemeSwitch lang={lang} />
      </div>
      </>
    )
  }

  if (!account.name && !account.email) return null
  return (
    <div className="flex h-16 shrink-0 items-center gap-2 border-t border-border px-3">
      <div className="flex min-w-0 flex-1 items-center gap-2.5 px-2">
        <div className="min-w-0 flex-1">
          {account.name && <p className="truncate text-sm font-medium">{account.name}</p>}
        </div>
        {/* Leaving sits INSIDE the appearance group rather than beside it. Two
            controls of the same size, one framed and one not, read as one thing
            somebody has not finished aligning; and there is nothing else in this
            corner for it to belong to. It is absent on the personal edition,
            where there is nobody to sign out as. */}
        <ThemeSwitch lang={lang} onSignOut={account.onSignOut} />
      </div>
    </div>
  )
}
