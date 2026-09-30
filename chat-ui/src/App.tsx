import './App.css'

import {
  Conversation,
  ConversationScrollButton,
  ConversationKeyboardAutoScroll,
  ConversationFollowsWhatYouSend,
} from '@/components/ui/ai/conversation';
import { Loader2, PaperclipIcon, Mic, ArrowUp, RotateCcwIcon, Maximize2Icon, Minimize2Icon, Minus, X, PanelLeft, Plus, ShieldCheck, Shield } from 'lucide-react';
import {
  PromptInput,
  PromptInputButton,
  PromptInputSubmit,
  PromptInputTextarea,
  PromptInputToolbar,
  PromptInputTools,
} from '@/components/ui/ai/prompt-input';
import { Button } from '@/components/ui/button';
import { ChatErrorBoundary } from '@/components/ui/ai/chat-error-boundary';
import { useCallback, useState, useEffect, useMemo, useRef } from 'react';
import { useChatStream } from '@lib/use-chat-stream';
import { useChatList } from '@lib/use-chat-list';
import { canAttach, pickerFilter, whyUnavailable } from '@lib/use-chat-accepts';
import { useRecorder, canRecord, clock, METER_BARS } from '@lib/use-recorder';
import { apiUpload } from '@lib/api';
import { resolveChatStore, chatsEndpointFor, isMultiChatEnabled } from '@lib/chat-mode';
import { ChatSidebar } from '@/components/ChatSidebar';
import { DelegationRail } from '@/components/DelegationRail';
import { AgentView } from '@/components/AgentView';
import { MessageItem, IDLE, type ChatTheme } from '@/components/MessageItem';
import { getFileTypeInfo, isImageFile } from '@/components/attachments';
import { ContextLine } from '@/components/ContextLine';
import { isAssistantMessage } from '@lib/chat-types';
import { rendersNothing } from '@lib/message-parts';
import type { FileAttachment, MessagePart } from '@lib/chat-types';
import type { ClientToolRegistry } from '@lib/client-tool-dispatcher';
import { t, resolveDynamic } from '@lib/utils';

/**
 * How loud it has been, as bars.
 *
 * It answers the one question a timer cannot: is the microphone actually
 * hearing me. A muted input counts the seconds up just as happily as a working
 * one, and somebody talking to a dead microphone should find out now rather
 * than after they finish their sentence.
 *
 * The bars that have not happened yet are dots, so the strip is the same width
 * throughout and the recording appears to fill it from the right.
 */
function Waveform({ levels }: { levels: number[] }) {
  const bars = Array.from({ length: METER_BARS }, (_, i) => {
    const at = i - (METER_BARS - levels.length);
    return at >= 0 ? levels[at] : null;
  });
  return (
    // justify-between rather than a fixed gap: the bars are a fixed thin width
    // and the SPACE between them is shared out, so the strip fills whatever
    // width it is given instead of ending halfway across.
    <div className="flex h-7 min-w-0 flex-1 items-center justify-between overflow-hidden">
      {bars.map((level, i) =>
        level === null ? (
          <span key={i} className="size-[2px] shrink-0 rounded-full bg-muted-foreground/30" />
        ) : (
          <span
            key={i}
            className="w-[2px] shrink-0 rounded-full bg-foreground/80"
            // A floor of 2px so a silent moment is still a mark: a gap reads as
            // the meter having stopped rather than as quiet.
            style={{ height: `${2 + level * 22}px` }}
          />
        ),
      )}
    </div>
  );
}

/**
 * Flatten an assistant message into ONE ordered list of timeline items —
 * reasoning blocks, text blocks, and individual tool rows — so live and
 * reloaded messages render through the exact same path with a single, uniform
 * spacing source.
 *
 * - Live stream: `message.parts` is already the ordered, interleaved list
 *   (reasoning / text / one tool per part) — use it verbatim.
 * - Reloaded (per-step) history: the message carries separate `reasoning`,
 *   `content`, and `tools[]` fields. Rebuild the same flat shape in the natural
 *   order the step produced them: think, respond, then the tools it ran — with
 *   each tool as its OWN item (never a nested group), so three tool calls sit at
 *   the same rhythm as everything else instead of bunching.
 */

/** Below this the list column does not fit beside a conversation. */
const SMALL_SCREEN = '(max-width: 767px)';

function isSmallScreen(): boolean {
  try {
    // A host that embeds the chat stamps this, because an iframe cannot see the
    // real viewport it is sitting in.
    if (document.documentElement.getAttribute('data-parent-mobile') === '1') return true;
    return window.matchMedia(SMALL_SCREEN).matches;
  } catch {
    return false;
  }
}

interface FlexieChatProps {
  streamEndpoint?: string,
  fetchEndpoint?: string,
  // A turn outlives the page that asked for it, so the page needs a way back
  // to one in flight, and a way to stop it.
  attachEndpoint?: string,
  cancelEndpoint?: string,
  resetEndpoint?: string,
  uploadEndpoint?: string,
  /** Base path for multi-thread chat history (list/create/update/delete are
   * derived). Omit to disable the sidebar and keep the single-chat behaviour. */
  chatsEndpoint?: string,
  /** Where the chat is running. `"flexie"` (default) = inside the product:
   * multi-thread, DB-backed, sidebar (when `chatsEndpoint` is set). `"external"`
   * = embedded on a third-party page: single chat keyed by the local
   * `fx_chat_session`, never a sidebar (a hard guard even if `chatsEndpoint`
   * leaks in). Declares intent instead of inferring it from `chatsEndpoint`. */
  chatStore?: 'flexie' | 'external',
  supportsFiles?: boolean,
  /** Admin gateway switch for the file-upload UI. When `false`, the attach
   * button is hidden regardless of model support. When `true` or unset,
   * the UI defers to `supportsFiles` (the model's capability). */
  enableFilesUpload?: boolean,
  maxUploadSize?: number,
  /** Auth token posted as `body.t` on every request. May be a string, a sync
   * factory `() => string`, or an async factory `() => Promise<string>` —
   * the factory is called per request so it can refresh from an endpoint. */
  token?: string | (() => string | Promise<string>),
  extraData?: any,
  lang?: Record<string, string>,
  /**
   * Host-registered client tools the AI can invoke. Lib-mode hosts may pass
   * this prop or set `window.FlexieAiConfig.clientTools`. Iframe-mode hosts
   * set `window.FlexieAiConfig.clientTools` on the parent page — the embed
   * loader strips handlers before crossing into the iframe and bridges
   * invocations back via FlexieBridge.
   */
  clientTools?: ClientToolRegistry,
  /** Show the collapsible reasoning block on assistant messages. Default true. */
  showReasoning?: boolean,
  /** Show the per-step tool-usage timeline on assistant messages. Default true. */
  showTools?: boolean,
  /** Image URL applied as a tiled background behind the messages area. */
  chatBackground?: string,
  /** Per-role visual overrides (font size / color / background). */
  theme?: ChatTheme,
  /** Show/hide the top-header buttons. All default `true`. */
  headerButtons?: {
    reset?: boolean,
    expand?: boolean,
    close?: boolean,
  },
  /** SAG: the chat is its own product, so it can sign you out. */
  onSignOut?: () => void,
  /** SAG: the workspaces the person may enter, for the sidebar's switcher. */
  workspaces?: { id: number, name: string }[],
  /** SAG: the workspace the chat is currently scoped to. */
  currentWorkspaceId?: number,
  /** SAG: move to another workspace the person belongs to. */
  onSwitchWorkspace?: (workspaceId: number) => void,
  /** SAG: who is signed in, shown at the foot of the sidebar. */
  user?: { name?: string, email?: string },
}

declare global {
  interface Window {
    FlexieAiConfig?: {
      streamEndpoint?: string,
      fetchEndpoint?: string,
      attachEndpoint?: string,
      cancelEndpoint?: string,
      resetEndpoint?: string,
      uploadEndpoint?: string,
      chatsEndpoint?: string,
      chatStore?: 'flexie' | 'external',
      supportsFiles?: boolean,
      enableFilesUpload?: boolean,
      maxUploadSize?: number,
      token?: string | (() => string | Promise<string>),
      extraData?: any,
      lang?: Record<string, string>,
      // Host-registered client tools (lib mode same-window OR iframe parent).
      // In iframe mode, the embed loader strips handlers from this map before
      // injecting config into the iframe — only metadata travels across.
      clientTools?: ClientToolRegistry,
      showReasoning?: boolean,
      showTools?: boolean,
      chatBackground?: string,
      theme?: ChatTheme,
      headerButtons?: {
        reset?: boolean,
        expand?: boolean,
        close?: boolean,
      },
    },
    toggleFlexieAiAgent?: () => void,
    toggleFlexieExpand?: () => void,
    FlexieBridge?: {
      send: (type: string, payload?: any) => void
      on: (type: string, fn: (...args: any[]) => void) => void
    }
  }
}


/** Where the chat talks to the gateway when nothing else says. */
const DEFAULT_STREAM_ENDPOINT = '/v1/chat/stream'
const DEFAULT_FETCH_ENDPOINT = '/v1/chat/history'

const FlexieAiAgent: React.FC<FlexieChatProps> = ({ streamEndpoint, fetchEndpoint, attachEndpoint, cancelEndpoint, resetEndpoint, uploadEndpoint, chatsEndpoint, chatStore, supportsFiles, enableFilesUpload, maxUploadSize, token, extraData, lang, clientTools, showReasoning, showTools, chatBackground, theme, headerButtons, onSignOut, workspaces, currentWorkspaceId, onSwitchWorkspace, user }) => {
  // Initialize config from window or props
  const [config, setConfig] = useState(() => ({
    // The two the chat cannot run without, so they always resolve to something.
    // An embed that configures neither gets the product's own routes rather than
    // `undefined` handed to fetch, which fails as "Failed to parse URL" three
    // frames away from the mistake.
    streamEndpoint: streamEndpoint || window.FlexieAiConfig?.streamEndpoint || DEFAULT_STREAM_ENDPOINT,
    fetchEndpoint: fetchEndpoint || window.FlexieAiConfig?.fetchEndpoint || DEFAULT_FETCH_ENDPOINT,
    attachEndpoint: attachEndpoint || window.FlexieAiConfig?.attachEndpoint,
    cancelEndpoint: cancelEndpoint || window.FlexieAiConfig?.cancelEndpoint,
    resetEndpoint: resetEndpoint || window.FlexieAiConfig?.resetEndpoint,
    uploadEndpoint: uploadEndpoint || window.FlexieAiConfig?.uploadEndpoint,
    chatsEndpoint: chatsEndpoint || window.FlexieAiConfig?.chatsEndpoint,
    // Product ("flexie") unless the embed explicitly opts out to "external".
    chatStore: resolveChatStore(chatStore, window.FlexieAiConfig?.chatStore),
    supportsFiles: supportsFiles ?? window.FlexieAiConfig?.supportsFiles ?? false,
    enableFilesUpload: enableFilesUpload ?? window.FlexieAiConfig?.enableFilesUpload,
    maxUploadSize: maxUploadSize ?? window.FlexieAiConfig?.maxUploadSize ?? 20 * 1024 * 1024,
    token: token || window.FlexieAiConfig?.token,
    extraData: extraData || window.FlexieAiConfig?.extraData,
    lang: lang || window.FlexieAiConfig?.lang,
    // Resolution order: prop → FlexieAiConfig.clientTools. The hook's
    // resolveLocalRegistry repeats this fallback for the live value at
    // handler-invocation time, so swapping window.FlexieAiConfig.clientTools
    // after mount (e.g. on page navigation) still works.
    clientTools: clientTools || window.FlexieAiConfig?.clientTools,
    showReasoning: showReasoning ?? window.FlexieAiConfig?.showReasoning ?? true,
    showTools: showTools ?? window.FlexieAiConfig?.showTools ?? true,
    chatBackground: chatBackground || window.FlexieAiConfig?.chatBackground,
    theme: theme || window.FlexieAiConfig?.theme,
    headerButtons: headerButtons || window.FlexieAiConfig?.headerButtons,
  }));

  // ─── Multi-thread chat (DB-backed). Enabled only inside the product
  // (chatStore "flexie") AND when a chatsEndpoint is configured. "external"
  // embeds are single-chat by declaration — chatId stays null and useChatStream
  // behaves as the legacy single session, regardless of chatsEndpoint. ───────
  const chatList = useChatList(chatsEndpointFor(config.chatStore, config.chatsEndpoint));
  const multiChatEnabled = isMultiChatEnabled(config.chatStore, chatList.enabled);
  const [currentChatId, setCurrentChatId] = useState<string | null>(null);
  // Which chip is open to read, by its id; null when none is.
  const [openedAgent, setOpenedAgent] = useState<string | null>(null);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [ensuredOnce, setEnsuredOnce] = useState(false);
  const ensuringRef = useRef(false);

  // useChatList returns a fresh object each render; keep refresh in a ref so the
  // turn-end effect can depend only on isStreaming.
  const chatRefreshRef = useRef(chatList.refresh);
  chatRefreshRef.current = chatList.refresh;
  const currentChatIdRef = useRef<string | null>(currentChatId);
  currentChatIdRef.current = currentChatId;
  // Chats created THIS session whose async title hasn't landed yet. This flag is
  // the whole mechanism: we look for the title only while a chat sits in here, and
  // drop it the instant the title arrives — so we never refetch on every prompt.
  const awaitingTitleRef = useRef<Set<string>>(new Set());

  // Draft first-prompt: the server created the chat and streamed its uid back.
  // Adopt it + show it in the list immediately, and mark it as awaiting the title
  // the background generator will produce.
  const handleChatCreated = useCallback((uid: string) => {
    setCurrentChatId(uid);
    chatList.addChatOptimistic(uid);
    awaitingTitleRef.current.add(uid);
  }, [chatList]);

  // The moment a title lands in the list, drop the chat from the awaiting set so
  // we stop looking for it. Runs whenever the list changes.
  useEffect(() => {
    if (awaitingTitleRef.current.size === 0) return;
    for (const c of chatList.chats) {
      if (c.title && awaitingTitleRef.current.has(c.id)) awaitingTitleRef.current.delete(c.id);
    }
  }, [chatList.chats]);

  // Block sending only while bootstrap is still resolving (so the first message is
  // never orphaned under the legacy key). A draft "New chat" (ensuredOnce, no
  // currentChatId) is allowed to send — that send creates the chat.
  const chatNotReady = multiChatEnabled && !currentChatId && !ensuredOnce;

  // `accepts` comes from the conversation's own answer: what may be attached and
  // whether anything may be spoken are read off the Gateway's configuration, and
  // the history request already carries them.
  const { messages, isStreaming, sendMessage, stop, reset, activity, respondToConfirmation, approvalMode, setApprovalMode, delegations, cancelDelegation, accepts, hasOlder, loadingOlder, loadOlder, meter, compact } = useChatStream(
    config.streamEndpoint,
    config.fetchEndpoint,
    config.attachEndpoint,
    config.cancelEndpoint,
    config.resetEndpoint,
    config.token,
    config.extraData,
    config.lang,
    config.clientTools,
    // Legacy mode passes undefined (fetch with the per-user key). Multi-chat mode
    // passes currentChatId, which is null in a draft "New chat" — the hook shows
    // an empty transcript and waits.
    multiChatEnabled ? currentChatId : undefined,
    // Draft: no chat resolved yet in multi-chat -> the next send creates one.
    multiChatEnabled && !currentChatId,
    handleChatCreated
  );

  // What the conversation actually draws.
  //
  // A message that renders nothing is not free: it still takes a row, and a row
  // takes the space after it. Filtered here rather than inside the list, so the
  // list only ever counts, measures and positions things that exist.
  const visibleMessages = useMemo(
    () => messages.filter((m) => !(isAssistantMessage(m) && rendersNothing(m))),
    [messages],
  );

  // The newest thing the PERSON said, which is what the conversation follows.
  //
  // Derived from the transcript rather than set by the send handler, because
  // there is more than one way to send: the composer, a suggested prompt inside
  // an answer, dictation. All of them end here.
  const lastFromYou = useMemo(() => {
    for (let i = messages.length - 1; i >= 0; i--) {
      if (messages[i].role === 'user') return messages[i].id;
    }
    return undefined;
  }, [messages]);

  // Turn-end: if the open chat is still awaiting its async title, refresh the list
  // once to pick it up. The awaiting flag guards this — the moment the title arrives the
  // chat is dropped from the set (above) and we never refetch again, so this fires at
  // most a couple of times per new chat, not on every prompt.
  useEffect(() => {
    if (isStreaming) return;
    const cid = currentChatIdRef.current;
    if (cid && awaitingTitleRef.current.has(cid)) chatRefreshRef.current();
  }, [isStreaming]);

  useEffect(() => {
    const handleMessage = (e: MessageEvent) => {
      const d = e.data;
      if (!d || !d.__flexie__ || d.type !== 'configUpdate') return;

      // Keep window.FlexieAiConfig in sync for consistency
      setConfig(prev => {
        const merged = { ...prev, ...d.payload };

        if (typeof window !== 'undefined') {
          window.FlexieAiConfig = { ...(window.FlexieAiConfig || {}), ...merged };
        }

        return merged;
      });

      console.log('[FlexieAiChat] Config updated via parent FlexieAiEmbedChat.updateConfig', d.payload);
    };

    window.addEventListener('message', handleMessage);
    return () => window.removeEventListener('message', handleMessage);
  }, []);

  // What is typed lives in a ref, not in state. As state, every keystroke
  // re-rendered this whole component, and some of its work grows with the
  // conversation: measured in WebKit, the engine the desktop app uses, a
  // keystroke in a long conversation took 23ms, and 11ms once typing re-rendered
  // nothing here (the message rows were already skipping; memoising the list
  // changed nothing). A ref and not the DOM, because recording unmounts the box
  // and what was typed must survive it. Only whether there is anything to send is
  // state, for the send button, and that changes twice a message.
  const draft = useRef('')
  const composerBox = useRef<HTMLTextAreaElement>(null)
  const [hasText, setHasText] = useState(false)
  const setDraft = useCallback((text: string) => {
    draft.current = text
    if (composerBox.current) composerBox.current.value = text
    setHasText(text.trim() !== '')
  }, [])
  // What the composer may offer, read from the Gateway's own configuration
  // rather than from a prop: a button that might turn out to do nothing is
  // worse than no button.

  // Why an upload was refused, shown above the composer. Never an alert(): a
  // modal for a file that was too large interrupts somebody mid-sentence.
  const [uploadError, setUploadError] = useState<string | null>(null)
  // Typing or talking. It is a MODE rather than a second input, because they are
  // two ways of saying the same thing and doing both at once means neither.
  const [composerMode, setComposerMode] = useState<'type' | 'talk'>('type')
  const [transcribing, setTranscribing] = useState(false)
  const recorder = useRecorder();
  // Restore the last size (full vs compact) so the expand icon and docked list
  // match the geometry the host restores on refresh. Written on every toggle.
  const [isExpanded, setIsExpanded] = useState(() => {
    try { return localStorage.getItem('fx_chat_expanded') === '1'; } catch { return false; }
  });
  const [pendingFiles, setPendingFiles] = useState<FileAttachment[]>([]);
  const [isUploading, setIsUploading] = useState(false);
  const fileInputRef = useRef<HTMLInputElement>(null);

  const handleExpandToggle = () => {
    const toggle = window.parent?.toggleFlexieExpand ?? window.toggleFlexieExpand;
    toggle?.();
    const next = !isExpanded;
    setIsExpanded(next);
    try { localStorage.setItem('fx_chat_expanded', next ? '1' : '0'); } catch { /* ignore */ }
  };

  // SAG: a small screen is a small screen, whether the chat is the whole page
  // or embedded in one. Two sources, one answer:
  //   - the viewport, for the standalone app (and the desktop/mobile shell);
  //   - the host's data-parent-mobile stamp, for when the chat is embedded in
  //     someone else's page and the iframe cannot see the real viewport.
  const [isParentMobile, setIsParentMobile] = useState(() => isSmallScreen());
  useEffect(() => {
    const el = document.documentElement;
    const read = () => setIsParentMobile(isSmallScreen());

    read();
    const query = window.matchMedia(SMALL_SCREEN);
    query.addEventListener('change', read);

    const obs = new MutationObserver(read);
    obs.observe(el, { attributes: true, attributeFilter: ['data-parent-mobile'] });

    return () => {
      query.removeEventListener('change', read);
      obs.disconnect();
    };
  }, []);

  // The chat list is part of the product, not a panel you go and find. On any
  // screen with room for it, it is simply there; the toggle exists only where
  // it does not fit. `isExpanded` is deliberately not consulted: it belonged to
  // an embedded widget that could be shrunk, and SAG's chat is the whole page.
  const docked = !isParentMobile;

  const handleAgentAiToggle = () => {
    const toggle = window.parent?.toggleFlexieAiAgent ?? window.toggleFlexieAiAgent;
    toggle?.();
  }

  // Core upload path, shared by the file picker AND clipboard paste. Takes a plain
  // File[] so callers only need to produce files (from an <input>, a drop, or a
  // pasted screenshot) and this handles size-check, preview, upload and pending state.
  /**
   * Uploading what somebody attached.
   *
   * One request per file, because the server keeps one file per attachment and
   * gives each its own id; those ids are what ride along with the message.
   *
   * A file too large is turned away HERE, before it is sent, so nobody spends
   * their upstream on something the server will refuse. The ceiling comes from
   * the server rather than from a number in this file: two places holding one
   * limit is two places to disagree.
   */
  const uploadFiles = useCallback(async (files: File[]) => {
    if (files.length === 0) return;
    setUploadError(null);

    const tooBig = files.find((f) => accepts.maxBytes > 0 && f.size > accepts.maxBytes);
    if (tooBig) {
      setUploadError(t('file_too_large', config.lang, '{name} is larger than {mb}MB', {
        name: tooBig.name,
        mb: Math.round(accepts.maxBytes / 1024 / 1024),
      }));
      return;
    }

    setIsUploading(true);
    try {
      for (const file of files) {
        // The preview is made before the request and thrown away if it fails,
        // so a refused upload leaves no blob URL behind.
        const previewUrl = file.type.startsWith('image/') ? URL.createObjectURL(file) : undefined;
        try {
          const res = await apiUpload('/v1/chat/uploads', file);
          if (!res.ok) {
            const body = await res.json().catch(() => ({}));
            if (previewUrl) URL.revokeObjectURL(previewUrl);
            setUploadError(body.error_description || t('file_upload_failed', config.lang, 'File upload failed'));
            break;
          }
          const stored = (await res.json()) as FileAttachment;
          setPendingFiles((prev) => [...prev, { ...stored, previewUrl }]);
        } catch {
          if (previewUrl) URL.revokeObjectURL(previewUrl);
          setUploadError(t('file_upload_failed', config.lang, 'File upload failed'));
          break;
        }
      }
    } finally {
      setIsUploading(false);
    }
  }, [accepts.maxBytes, config.lang]);

  const handleFileSelect = useCallback(async (e: React.ChangeEvent<HTMLInputElement>) => {
    const fileList = e.target.files;
    if (!fileList || fileList.length === 0) return;
    // Snapshot the FileList into a plain array BEFORE resetting the input,
    // because FileList is a live DOM object that gets emptied on reset.
    const files = Array.from(fileList);
    if (fileInputRef.current) fileInputRef.current.value = '';
    await uploadFiles(files);
  }, [uploadFiles]);

  // Clipboard paste: a copied image or a Windows screenshot (Snipping Tool /
  // PrtScn) lands on the clipboard as an image file, so pasting anywhere in the
  // chat attaches it straight away, no file browsing. Text paste is untouched.
  useEffect(() => {
    if (!canAttach(accepts)) return;
    const onPaste = (e: ClipboardEvent) => {
      const dt = e.clipboardData;
      if (!dt) return;
      const pasted: File[] = [];
      if (dt.items && dt.items.length) {
        for (const item of Array.from(dt.items)) {
          if (item.kind === 'file') {
            const f = item.getAsFile();
            if (f) pasted.push(f);
          }
        }
      }
      // Copied files (e.g. from the OS file manager) can arrive on .files instead.
      if (pasted.length === 0 && dt.files && dt.files.length) {
        pasted.push(...Array.from(dt.files));
      }
      if (pasted.length === 0) return; // plain text paste — let it through
      e.preventDefault();
      // Screenshots arrive unnamed or as a generic "image.png"; give each a unique,
      // meaningful name so repeated pastes don't collide on the name-keyed preview.
      const named = pasted.map((f, i) => {
        const generic = !f.name || f.name === 'image.png';
        if (!generic) return f;
        const type = f.type || 'image/png';
        const ext = (type.split('/')[1] || 'png').split('+')[0];
        return new File([f], `pasted-${Date.now()}-${i}.${ext}`, { type });
      });
      void uploadFiles(named);
    };
    document.addEventListener('paste', onPaste);
    return () => document.removeEventListener('paste', onPaste);
  }, [accepts, uploadFiles]);

  const removeFile = useCallback((fileId: string) => {
    setPendingFiles(prev => {
      const file = prev.find(f => f.id === fileId);
      if (file?.previewUrl) URL.revokeObjectURL(file.previewUrl);
      return prev.filter(f => f.id !== fileId);
    });
  }, []);

  /**
   * Talking instead of typing.
   *
   * Starting drops whatever was half-typed? No: the words already there are
   * kept and the transcription is appended, because somebody who typed a
   * sentence and then decided to speak the rest meant to keep both.
   */
  const startTalking = useCallback(async () => {
    setUploadError(null);
    setComposerMode('talk');
    await recorder.start();
  }, [recorder]);

  /**
   * Sending what was said.
   *
   * The recording goes to the model set up for audio, that model returns the
   * words, and the words go to the Gateway as the message. It is the same
   * journey a file makes: routed to the model that can read it, and what comes
   * back is fed straight to the Gateway rather than shown to the person first.
   *
   * Anything already typed goes with it rather than being left behind: somebody
   * who wrote half a sentence and then spoke the rest meant both, and silently
   * sending only one of them would be a poor guess.
   */
  const stopTalking = useCallback(async () => {
    const recording = await recorder.stop();
    setComposerMode('type');
    if (!recording) return;

    setTranscribing(true);
    try {
      const res = await apiUpload('/v1/chat/transcribe', recording);
      const body = await res.json().catch(() => ({}));
      if (!res.ok) {
        setUploadError(body.error_description || t('transcribe_failed', config.lang, 'That recording could not be turned into words'));
        return;
      }
      const said = String(body.text || '').trim();
      if (!said) {
        setUploadError(t('nothing_heard', config.lang, 'Nothing was heard in that recording'));
        return;
      }
      const typed = draft.current.trim();
      const message = typed ? `${typed} ${said}` : said;
      setDraft('');
      sendMessage(message);
    } catch {
      setUploadError(t('transcribe_failed', config.lang, 'That recording could not be turned into words'));
    } finally {
      setTranscribing(false);
    }
  }, [recorder, config.lang, setDraft, sendMessage]);

  const cancelTalking = useCallback(() => {
    recorder.cancel();
    setComposerMode('type');
  }, [recorder]);

  const handleSubmit = useCallback((event: React.SyntheticEvent<HTMLFormElement>) => {
    event.preventDefault()
    // `isStreaming` is deliberately NOT a reason to refuse. A conversation still
    // answers one thing at a time, but what is typed mid-answer now goes into
    // the running turn rather than being dropped, and the person usually typed
    // it BECAUSE of what they can see happening.
    const typed = draft.current
    if (!typed.trim() || isUploading || chatNotReady) return

    const attachments = pendingFiles.length > 0 ? [...pendingFiles] : undefined;
    sendMessage(typed, attachments)
    setDraft('')
    // The previews are blob URLs this document holds. Sending ends their life
    // as surely as removing one does, and only removal used to revoke them, so
    // every file actually sent leaked one.
    pendingFiles.forEach((f) => { if (f.previewUrl) URL.revokeObjectURL(f.previewUrl) })
    setPendingFiles([])
    setUploadError(null)
  }, [setDraft, isUploading, sendMessage, pendingFiles, chatNotReady])

  const handleReset = useCallback(() => {
    reset()
    setDraft('')
  }, [reset, setDraft])

  // One-time bootstrap: pick the last-opened chat, else the most-recent one. If
  // the user has NO chats we stay in a draft "New chat" (currentChatId = null)
  // instead of creating an empty row — the chat is created lazily on the first
  // prompt. Guarded by ensuredOnce so it runs once; post-delete re-selection is
  // handled in handleDeleteChat. The list endpoint lazily backfills a legacy
  // conversation on its first call, so nothing is lost when the sidebar turns on.
  useEffect(() => {
    if (!multiChatEnabled || currentChatId || !chatList.loaded || ensuredOnce) return;
    const stored = localStorage.getItem('fx_chat_current_id') || '';
    if (stored && chatList.chats.some((c) => c.id === stored)) setCurrentChatId(stored);
    else if (chatList.chats.length > 0) setCurrentChatId(chatList.chats[0].id);
    // else: draft — leave currentChatId null.
    setEnsuredOnce(true);
  }, [multiChatEnabled, currentChatId, chatList.loaded, chatList.chats, ensuredOnce]);

  useEffect(() => {
    if (currentChatId) localStorage.setItem('fx_chat_current_id', String(currentChatId));
  }, [currentChatId]);

  const handleSelectChat = useCallback((id: string) => {
    setCurrentChatId(id);
    setSidebarOpen(false);
  }, []);

  const handleNewChat = useCallback(() => {
    // Draft: no DB row yet — clearing currentChatId shows an empty transcript.
    // The chat is created on the first prompt (server streams chat_created back),
    // so clicking "New chat" repeatedly never piles up empty chats.
    setCurrentChatId(null);
    setSidebarOpen(false);
    // Focus the composer so you can start typing straight away. Desktop only —
    // auto-popping the mobile keyboard on "new chat" is intrusive. rAF so the
    // focus lands after the state change re-renders the (empty) transcript.
    if (!isParentMobile) {
      requestAnimationFrame(() => {
        const ta = document.querySelector('textarea[name="message"]') as HTMLTextAreaElement | null;
        ta?.focus();
      });
    }
  }, [isParentMobile]);

  const handleDeleteChat = useCallback(async (id: string) => {
    // If the open chat is being deleted, switch to another existing chat, else
    // drop to a draft "New chat". Pick from the current list before the refresh.
    setCurrentChatId((cur) => {
      if (cur !== id) return cur;
      const next = chatList.chats.find((c) => c.id !== id);
      return next ? next.id : null;
    });
    await chatList.deleteChat(id);
  }, [chatList]);

  return (
    // No border and no shadow on the root.
    //
    // It had both, and they were a frame around a page that fills the window.
    // Measured against the console: the chat's sidebar sat at x=1, y=1 and was
    // 898 tall where the console's was at 0,0 and 900, so every row in the
    // column was a pixel across and the foot of it two pixels up. Moving
    // between the two products moved the whole sidebar, which is the thing
    // somebody sees without being able to name it.
    <div className="fx-app-root relative flex h-full max-h-full w-full overflow-hidden bg-background">
      {/* Docked chat list — a permanent left column in full-screen (expanded)
          mode, ChatGPT-style. On a small screen it becomes the overlay drawer
          rendered below instead. */}
      {multiChatEnabled && docked && (
        <ChatSidebar
          docked
          open
          onClose={() => {}}
          list={chatList}
          currentChatId={currentChatId}
          onSelect={handleSelectChat}
          onNewChat={handleNewChat}
          onDeleteChat={handleDeleteChat}
          lang={config.lang}
          account={{
            name: user?.name || '',
            email: user?.email,
            workspaces,
            currentWorkspaceId,
            onSwitchWorkspace,
            onSignOut,
          }}
        />
      )}

      {/* Main column: header + conversation + input */}
      <div className="relative flex h-full min-w-0 flex-1 flex-col overflow-hidden">
      {/* The header, when there is anything left to put in it.
          
          There used to be a bar across the top of every conversation carrying a
          title, the workspace, and the way out. Those three are about the
          ACCOUNT rather than about the chat, and they live at the foot of the
          sidebar now, where a person looks for them. What is left here is what
          genuinely belongs to this pane: the embed's own window controls, the
          button that reveals the chat list where it does not fit, and Reset for
          a single-conversation embed. When a deployment has none of those, the
          bar is not rendered at all rather than rendered empty, and the
          conversation starts at the top of its own room. */}
      {(() => {
        // Default-true visibility flags; separators only render between two
        // visible buttons so we don't leave an orphan divider.
        const showReset = config.headerButtons?.reset !== false;
        const showExpand = config.headerButtons?.expand !== false && !isParentMobile;
        const showClose = config.headerButtons?.close !== false;
        const showSidebarToggle = multiChatEnabled && !docked;
        if (!showReset && !showExpand && !showClose && !showSidebarToggle) return null;

        return (
          <div className="flex shrink-0 items-center justify-end gap-2 border-b bg-muted/50 px-4 py-2">
            {/* The toggle exists only where the list does not fit. On any
                screen with room for it the list is simply there, so a button
                to reveal it would be a button to reveal something visible. */}
            {showSidebarToggle && (
              <Button variant="ghost" size="sm" onClick={() => setSidebarOpen(true)} className="h-8 px-2" title={t('chat_history', config.lang, 'Chats')}>
                <PanelLeft className="size-4" />
              </Button>
            )}

            {showReset && (
              multiChatEnabled ? (
                <Button variant="ghost" size="sm" onClick={handleNewChat} className="h-8 px-2">
                  <Plus className="size-4" />
                  <span className="ml-1 [html[data-parent-mobile='1']_&]:hidden">{t('chat_new', config.lang, 'New chat')}</span>
                </Button>
              ) : (
                <Button variant="ghost" size="sm" onClick={handleReset} className="h-8 px-2">
                  <RotateCcwIcon className="size-4" />
                  <span className="ml-1 [html[data-parent-mobile='1']_&]:hidden">{t('header_reset', config.lang, 'Reset')}</span>
                </Button>
              )
            )}

            {showReset && showExpand && (
              <div className="h-5 w-[1.5px] bg-gray-400 dark:bg-gray-500 mx-1 rounded-sm expand-separator" />
            )}

            {showExpand && (
              <Button variant="ghost" size="sm" className="h-8 px-2 expand-btn" onClick={handleExpandToggle}>
                {isExpanded ? <Minimize2Icon className="size-4" /> : <Maximize2Icon className="size-4" />}
              </Button>
            )}

            {(showExpand || showReset) && showClose && (
              <div className="h-5 w-[1.5px] bg-gray-400 dark:bg-gray-500 mx-1 rounded-sm expand-separator" />
            )}

            {showClose && (
              <Button variant="ghost" size="sm" className="h-8 px-2" onClick={handleAgentAiToggle}>
                <X className='size-4' />
              </Button>
            )}
          </div>
        );
      })()}

      {/* Below the header: a row holding the chat and, on its right, the
          background-tasks column (Mode C), so the tasks live INSIDE the chat
          room, level with the conversation, not as an outside sibling. */}
      <div className="flex min-h-0 flex-1 overflow-hidden">
      {/* Background wraps messages + input dock so the pattern bleeds edge-to-edge
          underneath both — only the header sits above it. */}
      <div
        className="flex min-w-0 flex-1 flex-col"
        style={config.chatBackground ? { backgroundImage: `url(${config.chatBackground})`, backgroundRepeat: 'repeat' } : undefined}
      >
      {/* Conversation Area */}
      <ChatErrorBoundary>
        <Conversation
          className="flex-1 min-h-0"
          // Only what will actually draw. A turn that renders nothing still
          // got a row of its own, and a row costs the space after it whether or
          // not anything is in it: answering an approval appends exactly such a
          // placeholder for the resume to write into, and when the answer is a
          // tool call and no prose, nothing ever arrives. That ghost sat under
          // every answered approval and was the extra gap beneath it.
          items={visibleMessages}
          idOf={(m) => m.id}
          conversation={currentChatId}
          hasOlder={hasOlder}
          loadingOlder={loadingOlder}
          onLoadOlder={loadOlder}
          notice={(
            <div className="flex items-center gap-2 rounded-full border bg-background/95 px-3 py-1 text-xs text-muted-foreground shadow-sm">
              <Loader2 className="size-3 animate-spin" />
              {t('loading_earlier', config.lang, 'Loading earlier messages…')}
            </div>
          )}
          // Activity indicator: reasoning falls through to the Reasoning component
          // when showReasoning is on; when it's off we show a spinner instead so the
          // user isn't staring at an empty surface during the think phase.
          renderItem={(message) => (
            <MessageItem
              message={message}
              activity={message.isStreaming ? activity : IDLE}
              showReasoning={config.showReasoning}
              showTools={config.showTools}
              theme={config.theme}
              sendMessage={sendMessage}
              respondToConfirmation={respondToConfirmation}
              lang={config.lang}
            />
          )}
        >
          <ConversationScrollButton />
          <ConversationKeyboardAutoScroll />
          <ConversationFollowsWhatYouSend lastFromYou={lastFromYou} conversation={currentChatId} />
        </Conversation>
      </ChatErrorBoundary>
      {/* Input Area */}
      {/* <div className="border-t p-4"> */}
      <div className="fx-input-dock p-4 shrink-0 pt-0 pb-[calc(1rem+env(safe-area-inset-bottom,0px))]">
        <div className="w-full mx-auto max-w-[776px]">
        {/* File preview chips — ChatGPT-style cards */}
        {pendingFiles.length > 0 && (
          <div className="flex flex-wrap gap-2 mb-2 px-1">
            {pendingFiles.map((file) => {
              if (isImageFile(file)) {
                // Image thumbnail with overlay X
                return (
                  <div key={file.id} className="relative group">
                    <div className="w-16 h-16 rounded-xl overflow-hidden border border-border/60 shadow-sm">
                      {file.previewUrl ? (
                        <img src={file.previewUrl} alt={file.file_name} className="w-full h-full object-cover" />
                      ) : (
                        <div className="w-full h-full flex items-center justify-center bg-violet-50 text-violet-500">
                          <svg className="size-6" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor"><path fillRule="evenodd" d="M1 5.25A2.25 2.25 0 013.25 3h13.5A2.25 2.25 0 0119 5.25v9.5A2.25 2.25 0 0116.75 17H3.25A2.25 2.25 0 011 14.75v-9.5zm1.5 5.81V14.75c0 .414.336.75.75.75h13.5a.75.75 0 00.75-.75v-2.06l-2.22-2.22a.75.75 0 00-1.06 0L9.06 15.56 6.22 12.72a.75.75 0 00-1.06 0L2.5 11.06zM12 7a1 1 0 11-2 0 1 1 0 012 0z" clipRule="evenodd" /></svg>
                        </div>
                      )}
                    </div>
                    <button
                      type="button"
                      onClick={() => removeFile(file.id)}
                      className="absolute -top-1.5 -right-1.5 rounded-full bg-foreground/80 text-background p-0.5 opacity-0 group-hover:opacity-100 transition-opacity shadow-sm hover:bg-foreground"
                    >
                      <X className="size-3" />
                    </button>
                  </div>
                );
              }
              // Document card with colored type badge
              const info = getFileTypeInfo(file);
              return (
                <div key={file.id} className="relative group flex items-center gap-2.5 rounded-xl border border-border/60 bg-background shadow-sm px-3 py-2.5 max-w-[220px]">
                  <div className="shrink-0 w-9 h-9 rounded-lg flex items-center justify-center" style={{ backgroundColor: info.bg }}>
                    <span className="text-[0.65rem] font-bold leading-none" style={{ color: info.color }}>{info.label}</span>
                  </div>
                  <div className="min-w-0 flex-1">
                    <div className="text-xs font-medium text-foreground truncate leading-tight">{file.file_name}</div>
                    <div className="text-[0.65rem] text-muted-foreground leading-tight mt-0.5">{info.label}</div>
                  </div>
                  <button
                    type="button"
                    onClick={() => removeFile(file.id)}
                    className="absolute -top-1.5 -right-1.5 rounded-full bg-foreground/80 text-background p-0.5 opacity-0 group-hover:opacity-100 transition-opacity shadow-sm hover:bg-foreground"
                  >
                    <X className="size-3" />
                  </button>
                </div>
              );
            })}
          </div>
        )}
        {/* Why an upload was refused, said in place. A modal for a file that
            was too large interrupts somebody mid-sentence and tells them
            nothing they could not read here. */}
        {/* A vendor set up for files or audio that cannot do the job. The
            person reading this is often not the person who configured it, and
            "the button is missing" is not something they can report. */}
        {whyUnavailable(accepts) && (
          <div className="mb-2 rounded-md border border-border bg-muted/40 px-3 py-2">
            <span className="text-xs text-muted-foreground">{whyUnavailable(accepts)}</span>
          </div>
        )}
        {recorder.error && (
          <div className="mb-2 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2">
            <span className="text-xs text-destructive">{recorder.error}</span>
          </div>
        )}
        {uploadError && (
          <div className="mb-2 flex items-start justify-between gap-3 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2">
            <span className="text-xs text-destructive">{uploadError}</span>
            <button
              type="button"
              onClick={() => setUploadError(null)}
              className="text-xs text-muted-foreground hover:text-foreground"
              aria-label="Dismiss"
            >
              <X size={14} />
            </button>
          </div>
        )}
        <PromptInput onSubmit={handleSubmit}>
          {/* How full the conversation is, and the way to make room. Inside the
              composer, above what is being typed, so it never scrolls away and
              sits where the next message is decided. Only for a conversation
              that exists: a draft has nothing in it yet. */}
          {currentChatId && (
            <ContextLine
              meter={meter}
              busy={isStreaming}
              lang={config.lang}
              onCompact={() => {
                const base = chatsEndpointFor(config.chatStore, config.chatsEndpoint)
                if (base) void compact(base)
              }}
            />
          )}
          {composerMode === 'talk' ? (
            /* Recording. The textarea is UNMOUNTED rather than disabled: Enter
               inside it calls form.requestSubmit(), so one that is merely
               disabled still submits, and there is nothing to submit yet. */
            <div className="flex items-center gap-4 px-4 py-3">
              <button
                type="button"
                onClick={cancelTalking}
                className="shrink-0 text-muted-foreground transition-colors hover:text-foreground"
                title={t('cancel', config.lang, 'Cancel')}
                aria-label={t('cancel', config.lang, 'Cancel')}
              >
                <X size={18} />
              </button>

              <Waveform levels={recorder.levels} />

              <span className="shrink-0 text-xs tabular-nums text-muted-foreground">
                {clock(recorder.seconds)}
              </span>
              {/* SEND, not "done". Pressing this is the whole act: the
                  recording goes to the model set up for audio, and the words it
                  returns go to the Gateway as the message. Nothing comes back
                  to be looked over first, which is the same journey a file's
                  interpretation makes. */}
              <button
                type="button"
                onClick={() => void stopTalking()}
                disabled={recorder.state !== 'recording'}
                className="flex size-9 shrink-0 items-center justify-center rounded-full bg-foreground text-background transition-opacity hover:opacity-90 disabled:opacity-40"
                title={t('send', config.lang, 'Send')}
                aria-label={t('send', config.lang, 'Send')}
              >
                <ArrowUp size={18} />
              </button>
            </div>
          ) : (
            <PromptInputTextarea
              ref={composerBox}
              defaultValue={draft.current}
              onChange={(e) => {
                draft.current = e.target.value
                setHasText(e.target.value.trim() !== '')
              }}
              // Frozen while the conversation is compacted: what is typed now
              // would be sent into a conversation that is being rewritten.
              disabled={meter.compacting}
              placeholder={
                meter.compacting
                  ? t('compacting_placeholder', config.lang, 'Compacting this conversation…')
                  : transcribing
                  ? t('transcribing', config.lang, 'Sending what you said...')
                  : t('chat_placeholder', config.lang, 'How can I help you today?')
              }
            />
          )}
          {/* While recording, the recording IS the composer. The toolbar below
              it would offer a send button with nothing to send and settings for
              a message that does not exist yet, so it is not rendered at all. */}
          {composerMode === 'type' && (
          <PromptInputToolbar>
            <PromptInputTools>
              {/* The attach button is a reading of the Gateway's own
                  configuration, not a setting of its own: it appears when
                  something over there can actually read a file, and the picker
                  offers only the types a rule covers, so a file that would be
                  refused is never chosen in the first place. */}
              {composerMode === 'type' && canAttach(accepts) && (
                <>
                  <input
                    ref={fileInputRef}
                    type="file"
                    multiple
                    accept={pickerFilter(accepts)}
                    onChange={handleFileSelect}
                    className="hidden"
                  />
                  <PromptInputButton
                    disabled={isStreaming || isUploading}
                    onClick={() => fileInputRef.current?.click()}
                    type="button"
                  >
                    {isUploading ? (
                      <Loader2 className="size-4 animate-spin" />
                    ) : (
                      <PaperclipIcon size={16} />
                    )}
                    <span className='text-[0.8em]'>
                      {isUploading
                        ? t('uploading', config.lang, 'Uploading...')
                        : t('attach_files', config.lang, 'Attach files')}
                    </span>
                  </PromptInputButton>
                </>
              )}
              {/* Per-conversation approval mode: ask every time, or auto-approve
                  risky actions (gateway and agents) for the rest of this
                  session. Only meaningful once the conversation exists. */}
              {currentChatId && (
                <PromptInputButton
                  type="button"
                  onClick={() => setApprovalMode(approvalMode === 'auto' ? 'manual' : 'auto')}
                  title={approvalMode === 'auto'
                    ? t('auto_approve_on_hint', config.lang, 'This conversation runs approval-gated actions without asking. Click to require approval again.')
                    : t('auto_approve_off_hint', config.lang, 'Approval-gated actions ask first. Click to auto-approve them for this conversation.')}
                  className={approvalMode === 'auto' ? 'text-green-600 dark:text-green-400' : undefined}
                >
                  {approvalMode === 'auto' ? <ShieldCheck size={16} /> : <Shield size={16} />}
                  <span className='text-[0.8em]'>
                    {approvalMode === 'auto'
                      ? t('auto_approve_on', config.lang, 'Auto-approving')
                      : t('auto_approve_off', config.lang, 'Ask to approve')}
                  </span>
                </PromptInputButton>
              )}
            </PromptInputTools>
            {isStreaming && (
              <button
                type="button"
                onClick={stop}
                className="text-xs text-muted-foreground hover:text-foreground transition-colors cursor-pointer"
                style={{marginRight: '5px'}}
              >
                {t('cancel', config.lang, 'Cancel')}
              </button>
            )}
            {/* Talking instead of typing: the icon alone, beside send, because
                that is where the two ways of committing a message belong. It is
                offered only when the Gateway has a model that transcribes AND
                this browser has a microphone, since a button that asks for a
                permission it can do nothing with is worse than no button. */}
            {composerMode === 'type' && accepts.audio && canRecord() && (
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="mr-1 shrink-0 text-muted-foreground"
                disabled={isStreaming || isUploading || transcribing}
                onClick={() => void startTalking()}
                title={t('talk_hint', config.lang, 'Talk instead of typing')}
                aria-label={t('talk_hint', config.lang, 'Talk instead of typing')}
              >
                {transcribing ? <Loader2 className="size-4 animate-spin" /> : <Mic size={16} />}
              </Button>
            )}
            <PromptInputSubmit
              disabled={!hasText || isStreaming || isUploading || chatNotReady || meter.compacting}
              status={isStreaming ? 'streaming' : 'ready'}
            />
          </PromptInputToolbar>
          )}
        </PromptInput>
        <span className='fx-disclaimer text-[0.7em] text-muted-foreground block text-center mt-3'>
          {t('ai_mistake_warning', config.lang, 'AI can make mistakes. Check your AI vendor\'s terms & conditions.')}
        </span>
        </div>
      </div>
      </div>

      {/* The background-tasks column: right side of the chat room, a flex sibling
          of the conversation so it takes its own room level with the messages.
          Renders only while agents run. */}
      <DelegationRail delegations={delegations} onCancel={cancelDelegation} onOpen={(d) => setOpenedAgent(d.id)} />
      {/* An agent, or a batch, opened from its chip to read. Its chip is looked
          up by id on every render, so the socket's pushes keep it current and a
          conversation switched away closes it. */}
      <AgentView
        chip={delegations.find((d) => d.id === openedAgent) ?? null}
        chatId={currentChatId}
        historyEndpoint={config.fetchEndpoint}
        showReasoning={config.showReasoning}
        showTools={config.showTools}
        theme={config.theme}
        lang={config.lang}
        onClose={() => setOpenedAgent(null)}
      />
      </div>

      {/* Small screens: the list is an overlay drawer, opened from the header.
          (Anything wider keeps it docked as a permanent column, above.) */}
      {multiChatEnabled && !docked && (
        <ChatSidebar
          open={sidebarOpen}
          onClose={() => setSidebarOpen(false)}
          list={chatList}
          currentChatId={currentChatId}
          onSelect={handleSelectChat}
          onNewChat={handleNewChat}
          onDeleteChat={handleDeleteChat}
          lang={config.lang}
        />
      )}
      </div>
    </div>
  );
};

export default FlexieAiAgent;