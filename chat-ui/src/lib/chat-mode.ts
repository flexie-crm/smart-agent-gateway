// lib/chat-mode.ts
// ─── Chat mode: inside the Flexie product, or embedded on a third-party page? ──
//
// Declared explicitly via config `chatStore`, NOT inferred from `chatsEndpoint`:
//   "flexie"   (default) → multi-thread, DB-backed; sidebar when chatsEndpoint is set.
//   "external"           → single chat keyed by the local fx_chat_session; never a
//                          sidebar — a hard guard even if chatsEndpoint leaks in.

export type ChatStore = 'flexie' | 'external'

/** Resolve the mode: prop wins, then window config, then default "flexie"
 * (the product is the default context; external embeds opt out explicitly). */
export function resolveChatStore(fromProp?: ChatStore, fromWindow?: ChatStore): ChatStore {
  return fromProp || fromWindow || 'flexie'
}

/** The chats-list endpoint the sidebar reads. Suppressed entirely in external
 * mode so no list request is even made. */
export function chatsEndpointFor(store: ChatStore, chatsEndpoint?: string): string | undefined {
  return store === 'external' ? undefined : chatsEndpoint
}

/** Multi-thread sidebar is on only inside the product AND with a chats endpoint.
 * `chatsEndpointEnabled` is useChatList's own "endpoint configured" signal. */
export function isMultiChatEnabled(store: ChatStore, chatsEndpointEnabled: boolean): boolean {
  return store !== 'external' && chatsEndpointEnabled
}
