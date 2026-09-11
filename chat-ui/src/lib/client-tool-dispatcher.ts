// lib/client-tool-dispatcher.ts
// ─── Browser-side dispatcher for AI client tools ────────────────────────────
//
// Fire-and-forget. Triggered by a `client_tool_call` SSE frame. Resolves the
// host-registered handler and invokes it. The handler's return value is NOT
// sent back to the server — the SSE stream is one-way and the model already
// received its synthetic OK server-side.

import { validateClientToolArgs, type ClientToolParameters } from './client-tool-schema';

/** What the host registers per tool name. Mirrors the server-side def shape. */
export interface ClientToolEntry {
  description: string;
  parameters: ClientToolParameters;
  handler: (args: any) => unknown | Promise<unknown>;
  requiresConfirm?: boolean;
  friendlyName?: string;
}

export type ClientToolRegistry = Record<string, ClientToolEntry>;

export interface ClientToolInvokePayload {
  name: string;
  friendly_name?: string;
  args: Record<string, unknown>;
  requires_confirm?: boolean;
}

/**
 * Resolve the live registry. The chat does NOT know whether it is mounted in
 * the host page directly (lib) or inside an iframe (embed). Either way, the
 * host might register tools in this window OR in the parent window.
 *
 * Resolution order (first match wins):
 *   1. Explicit `propRegistry` (lib consumers passing `clientTools` directly)
 *   2. `window.FlexieAiConfig.clientTools` on this window
 *   3. `window.parent.FlexieAiConfig.clientTools` — same-origin parent only;
 *      cross-origin access throws and we silently fall through to the bridge.
 *
 * Cross-origin parent (the canonical iframe-embed case) is reached via the
 * `FlexieBridge` postMessage round-trip in {@see invokeLocalClientTool}; the
 * embed loader strips handlers from the in-iframe `FlexieAiConfig.clientTools`
 * so step (2) returns metadata-only entries — step (3) is skipped (throws)
 * and the dispatcher bridges instead.
 */
export function resolveLocalRegistry(propRegistry?: ClientToolRegistry): ClientToolRegistry {
  if (propRegistry && typeof propRegistry === 'object') return propRegistry;
  if (typeof window === 'undefined') return {};

  const own = (window as any).FlexieAiConfig;
  if (own && own.clientTools && typeof own.clientTools === 'object') {
    return own.clientTools as ClientToolRegistry;
  }

  if (window !== window.parent) {
    try {
      const parentCfg = (window.parent as any).FlexieAiConfig;
      if (parentCfg && parentCfg.clientTools && typeof parentCfg.clientTools === 'object') {
        return parentCfg.clientTools as ClientToolRegistry;
      }
    } catch {
      // Cross-origin — fall through to the bridge.
    }
  }

  return {};
}

/**
 * Invoke a client tool. Fire-and-forget — nothing is returned to the caller
 * and nothing is sent back to the server. Errors are logged to the host
 * console; the model already received its synthetic OK on the server side
 * and is continuing its narration over the open SSE.
 *
 * Resolution:
 *   1. Local registry entry with a callable `handler` → invoke in-place.
 *   2. Local registry entry with NO handler (iframe-embed mode — embed loader
 *      stripped handlers before crossing srcdoc) → forward to parent via
 *      `FlexieBridge.send('client_tool_invoke', …)`.
 *   3. Neither → console.warn and drop. The model has already moved on.
 */
export function invokeLocalClientTool(
  payload: ClientToolInvokePayload,
  registry: ClientToolRegistry
): void {
  const entry = registry[payload.name];

  if (entry && typeof entry.handler === 'function') {
    const validation = validateClientToolArgs(payload.args ?? {}, entry.parameters);
    if (!validation.ok) {
      console.warn('[FlexieClientTool] Arg validation failed for', payload.name, '-', validation.error);
      return;
    }
    try {
      const ret = entry.handler(payload.args ?? {});
      if (ret && typeof (ret as Promise<unknown>).then === 'function') {
        (ret as Promise<unknown>).catch((err) => {
          console.error('[FlexieClientTool] Handler rejected for', payload.name, err);
        });
      }
    } catch (err) {
      console.error('[FlexieClientTool] Handler threw for', payload.name, err);
    }
    return;
  }

  // No handler here — try the bridge (iframe-embed path).
  if (hasBridge()) {
    invokeViaBridge(payload);
    return;
  }

  console.warn('[FlexieClientTool] No handler registered for "' + payload.name + '" and no bridge available.');
}

function hasBridge(): boolean {
  return typeof window !== 'undefined'
    && !!(window as any).FlexieBridge
    && typeof (window as any).FlexieBridge.send === 'function';
}

function invokeViaBridge(payload: ClientToolInvokePayload): void {
  const bridge = (window as any).FlexieBridge;
  try {
    bridge.send('client_tool_invoke', {
      name: payload.name,
      friendly_name: payload.friendly_name,
      args: payload.args || {},
      requires_confirm: payload.requires_confirm,
    });
  } catch (err) {
    console.error('[FlexieClientTool] Bridge send failed for', payload.name, err);
  }
}
