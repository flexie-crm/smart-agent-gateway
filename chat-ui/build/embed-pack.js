import fs from 'fs'
import path from 'path'
import { fileURLToPath } from 'url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const dist = path.join(__dirname, '..', process.env.SAG_OUT_DIR || 'dist/app')

// detect if Vite output has an "assets" directory
const assetsDir = fs.existsSync(path.join(dist, 'assets'))
  ? path.join(dist, 'assets')
  : dist

// find JS and CSS files
const assets = fs.readdirSync(assetsDir)
const jsFile = assets.find(f => f.startsWith('index-') && f.endsWith('.js')) || assets.find(f => f.endsWith('.js'))
const cssFile = assets.find(f => f.endsWith('.css')) || null

if (!jsFile) {
  console.error('❌ Could not find JS bundle in', assetsDir)
  process.exit(1)
}

// adjust asset path suffix depending on build type
const assetBaseSuffix = assetsDir.endsWith('assets') ? '/assets/' : '/'

const embedCode = `
/**
 * Chat UI Embed Loader (universal version)
 *
 * Usage:
 * <script src="https://your.host/media/js/ai/ai-embed.js"></script>
 * <script>
 *   window.FlexieAiConfig = {
 *     // ────────── Endpoints ──────────
 *     streamEndpoint: "/s/ai/agent",        // POST: opens the SSE chat stream
 *     fetchEndpoint:  "/s/ai/chat",         // POST: returns chat history for session
 *     resetEndpoint:  "/s/ai/reset",        // POST: clears the current chat session
 *     uploadEndpoint: "/s/ai/upload",       // POST: file upload endpoint (when supportsFiles)
 *
 *     // ────────── Auth + custom payload ──────────
 *     token:     "",                         // Optional bearer/token passed in body.token
 *     extraData: {},                         // Arbitrary object sent with every turn as body.e
 *
 *     // ────────── File upload feature ──────────
 *     supportsFiles: false,                  // Toggles the file-attach UI
 *     maxUploadSize: 20971520,               // Bytes; default 20 MB
 *
 *     // ────────── Render mode ──────────
 *     renderMode: "iframe",                  // "iframe" (default) = floating, style-isolated
 *                                            //   widget injected into any host page.
 *                                            // "inline" = mount the chat DIRECTLY into this
 *                                            //   document (no iframe, no bridge). For a
 *                                            //   dedicated full-page chat / PWA where you own
 *                                            //   the page and its mobile UX. The host page
 *                                            //   provides the <meta viewport> and styling.
 *
 *     // ────────── Mount + visibility ──────────
 *     selector:             "body",          // iframe mode: element the frame is appended to.
 *                                            // inline mode: container that holds the chat — a
 *                                            //   #root is created inside it if the page has none.
 *     toggleButtonSelector: "#open-ai-agent", // Optional click target to toggle visibility
 *     autoOpen:             false,           // false = start hidden; true = visible on load
 *
 *     // ────────── Floating-frame appearance ──────────
 *     style: {                               // Overrides for the iframe's inline style
 *       bottom: "30px",
 *       right:  "30px",
 *       width:  "620px",
 *       height: "700px"
 *     },
 *
 *     // ────────── UI strings ──────────
 *     lang: {                                // Translatable strings consumed by the chat UI
 *       header_ai_agent: "AI Agent"
 *     },
 *
 *     // ────────── Client-side tools (optional) ──────────
 *     // Tools the AI can invoke in the host page. Each entry's parameter
 *     // schema is sent to the model so it can pick arguments correctly.
 *     // Handlers run in THIS window; for the iframe embed, the loader strips
 *     // handlers before crossing srcdoc and bridges invocations back here
 *     // via postMessage automatically. Set requiresConfirm: true for any
 *     // handler with destructive side effects.
 *     clientTools: {
 *       openRecord: {
 *         description: "Open a record in the current view. Use when the user asks to view/edit a record by ID.",
 *         parameters: {
 *           type: "object",
 *           properties: {
 *             entity: { type: "string", enum: ["contact", "lead", "case"] },
 *             id:     { type: "integer" }
 *           },
 *           required: ["entity", "id"]
 *         },
 *         friendlyName:    "Open record",
 *         requiresConfirm: false,
 *         handler: function (args) {
 *           window.location.href = "/" + args.entity + "/view/" + args.id;
 *           // Return value is discarded — side effects in the page are the
 *           // signal to the user. The AI already received its acknowledgement
 *           // on the server side.
 *         }
 *       },
 *       showToast: {
 *         description: "Show a short, non-blocking notification to the user.",
 *         parameters: {
 *           type: "object",
 *           properties: {
 *             message: { type: "string" },
 *             variant: { type: "string", enum: ["info", "success", "warning", "error"] }
 *           },
 *           required: ["message"]
 *         },
 *         handler: function (args) {
 *           // host implementation — fire-and-forget; return value is discarded.
 *           myToast.show(args.message, { variant: args.variant || "info" });
 *         }
 *       }
 *     }
 *   };
 * </script>
 *
 * Programmatic update after init:
 *   window.FlexieAiEmbedChat.updateConfig({ token: "new-token", extraData: {...} });
 *   // (clientTools handlers cannot be updated this way — set them on window.FlexieAiConfig.clientTools.)
 */

(function () {
  const ASSET_BASE = (window.FlexieAiConfig?.assetBase || '/media/js/ai${assetBaseSuffix}')
    .replace(/\\/$/, '') + '/';
  const JS_FILE = '${jsFile}';
  const CSS_FILE = ${cssFile ? `'${cssFile}'` : 'null'};

  // ---------- Auto-Bridge top/iframe -------------------
  (function initBridge(){
    const isIframe = window !== window.parent;
    const subscribers = {};
    const queue = [];

    const sendTarget = () => {
      if (isIframe) return window.parent;
      return window.FlexieIframe?.contentWindow || null;
    };

    function send(type, payload) {
      const msg = { __flexie__: true, type, payload };
      const target = sendTarget();
      if (target) target.postMessage(msg, '*');
      else queue.push(msg);
    }

    function on(type, fn) {
      (subscribers[type] = subscribers[type] || []).push(fn);
    }

    function emit(type, payload) {
      (subscribers['*'] || []).forEach(fn => fn(type, payload));
      (subscribers[type] || []).forEach(fn => fn(payload));
    }

    window.addEventListener('message', e => {
      const d = e.data;
      if(!d || !d.__flexie__) return;
      // Parent receiving iframe messages: srcdoc iframes report event.origin
      // as 'null' (opaque). On the parent side, only accept messages whose
      // source is our own iframe element — prevents arbitrary same-page
      // windows from spoofing client_tool replies. In-iframe receives only
      // come from window.parent (event.source === window.parent) which the
      // browser sets and cannot be spoofed.
      if (!isIframe) {
        if (!window.FlexieIframe || e.source !== window.FlexieIframe.contentWindow) {
          return;
        }
      } else {
        if (e.source !== window.parent) {
          return;
        }
      }
      emit(d.type, d.payload);
    });

    const obs = new MutationObserver(() => {
      if (!isIframe && window.FlexieIframe?.contentWindow && queue.length) {
        const t = sendTarget();
        if (t) {
          queue.forEach(m => t.postMessage(m, '*'));
          queue.length = 0;
        }

        obs.disconnect(); // Stop watching once iframe is found and flushed
      }
    });

    if (!isIframe) obs.observe(document.body, { childList: true, subtree: false });

    window.FlexieBridge = {send, on};
  })();
  // --------------------------------------------------------------

  // ---------- Parent-side Client-Tool Dispatcher ----------------
  // ai-embed.js runs in the host page (the parent / top window). The chat
  // iframe loads it own srcdoc scripts and never runs this loader. When the
  // AI invokes a client tool, the iframe sends a 'client_tool_invoke' message
  // and we resolve the registered handler from window.FlexieAiConfig.clientTools
  // and fire it. Fire-and-forget — no reply is sent back; the model already
  // got its synthetic OK on the server side.
  (function initClientToolBridge(){
    // Only register the parent-side listener when we ARE the parent.
    // window === window.parent is true for the top-level window.
    if (window !== window.parent) return;
    if (!window.FlexieBridge) return;

    window.FlexieBridge.on('client_tool_invoke', function(payload) {
      if (!payload || typeof payload.name !== 'string') return;

      var registry = (window.FlexieAiConfig && window.FlexieAiConfig.clientTools) || {};
      var entry = registry[payload.name];
      if (!entry || typeof entry.handler !== 'function') {
        console.warn('[FlexieClientTool] No handler registered for', payload.name);
        return;
      }

      try {
        // Fire-and-forget — return value is discarded.
        var ret = entry.handler(payload.args || {});
        if (ret && typeof ret.then === 'function') {
          ret.catch(function(err){
            console.error('[FlexieClientTool] Handler rejected for', payload.name, err);
          });
        }
      } catch (err) {
        console.error('[FlexieClientTool] Handler threw for', payload.name, err);
      }
    });
  })();
  // --------------------------------------------------------------

  // Strip non-serialisable bits from cfg before it crosses into the iframe
  // via srcdoc. clientTools entries carry function handlers which JSON.stringify
  // would silently drop — the iframe gets metadata only and dispatches handler
  // invocations back to the parent via FlexieBridge.
  function sanitizeCfgForIframe(cfg) {
    const out = {};
    for (const k in cfg) {
      if (!Object.prototype.hasOwnProperty.call(cfg, k)) continue;
      if (k === 'clientTools' && cfg.clientTools && typeof cfg.clientTools === 'object') {
        const stripped = {};
        for (const name in cfg.clientTools) {
          if (!Object.prototype.hasOwnProperty.call(cfg.clientTools, name)) continue;
          const entry = cfg.clientTools[name];
          if (!entry || typeof entry !== 'object') continue;
          stripped[name] = {
            description: entry.description,
            parameters: entry.parameters,
            friendlyName: entry.friendlyName,
            requiresConfirm: !!entry.requiresConfirm,
            // No 'handler' — bridged via FlexieBridge.
          };
        }
        out.clientTools = stripped;
        continue;
      }
      out[k] = cfg[k];
    }
    return out;
  }

  function createIframe(cfg, container) {
    const iframe = document.createElement('iframe');
    const defaultStyle = {
      position: 'fixed',
      bottom: '20px',
      right: '20px',
      width: '620px',
      height: '700px',
      maxHeight: 'calc(100vh - 40px)',
      border: 'none',
      borderRadius: '12px',
      boxShadow: '0 0 15px rgba(0,0,0,0.25)',
      zIndex: 1049,
      background: 'white',
      transition: 'none',
      transform: cfg.autoOpen === false ? 'translateY(110%)' : 'translateY(0)',
      opacity: cfg.autoOpen === false ? '0' : '1',
      // display:none from the FIRST moment (before append) when starting closed, so
      // there is never a frame where the iframe is a measurable box on the host page.
      // If the host reads layout synchronously on DOM insertion, it sees nothing.
      display: cfg.autoOpen === false ? 'none' : 'block',
    };

    Object.assign(iframe.style, defaultStyle, cfg.style || {});
    (container || document.body || document.documentElement).appendChild(iframe);

    // Add a class to iframe for reference
    iframe.className = 'flexie-chat-iframe';

    const style = document.createElement('style');
    style.textContent = \`
      @media (max-width: 768px) {
        /* THE OCCLUDER is this full-screen <div>, NOT the iframe. A div can actually
           fill the screen (top/left/width/height:100vw/100vh); an <iframe> is a
           replaced element and can't be stretched reliably. The backdrop permanently
           covers the host so nothing behind is ever visible, while the iframe is sized
           to the VISUAL viewport (above the keyboard) — which is what stops iOS from
           scrolling the focused textarea up. Any transient gap between the iframe and
           the keyboard (during a pan/animation) shows this white backdrop, not host. */
        .flexie-chat-backdrop {
          position: fixed !important;
          top: 0 !important;
          left: 0 !important;
          width: 100vw !important;
          height: 100vh !important;
          background: #fff !important;
          z-index: 1048 !important;
        }
        .flexie-chat-iframe {
          /* Geometry is JS-driven (glueIframe): the iframe tracks the VISUAL viewport
             (the rectangle above the keyboard). Sizing it to the visual viewport — not
             full screen — is what keeps iOS from scrolling the textarea up. Sits on top
             of the backdrop (z 1049 > 1048). */
          position: fixed !important;
          max-height: none !important;
          border-radius: 0 !important;
          box-shadow: none !important;
          z-index: 1049 !important;
        }
      }
    \`;

    // Add this styling to head
    document.head.appendChild(style);

    iframe.srcdoc = \`<!DOCTYPE html>
      <html lang="en">
        <head>
          <meta charset="UTF-8" />
          <meta name="viewport" content="width=device-width, initial-scale=1, maximum-scale=1, viewport-fit=cover" />
          <base href="\${ASSET_BASE}">
          \${CSS_FILE ? \`<link rel="stylesheet" href="\${ASSET_BASE + CSS_FILE}">\` : ''}
          <style id="parent-responsive-style"><\\/style>
          <style>
            html, body, #root {
              height: 100%;
              margin: 0;
            }
            /* MOBILE: the iframe ELEMENT is sized by JS to the visual viewport (above
               the keyboard); the chat app fills it. --fx-vh is the same height,
               relayed by the parent, as a belt-and-suspenders clamp in case iOS reports
               the iframe's own inner height stale. Desktop leaves --fx-vh unset → 100%. */
            html, body, #root, .fx-app-root {
              height: var(--fx-vh, 100%) !important;
              max-height: var(--fx-vh, 100%) !important;
            }
            /* Only the conversation scrolls — lock the document itself. position:fixed
               on body stops iOS from scrolling the whole iframe up to reveal the
               focused input (which sits at the bottom behind the keyboard). */
            html, body {
              overflow: hidden;
              overscroll-behavior: none;
            }
            body {
              position: fixed;
              top: 0;
              left: 0;
              right: 0;
            }
            /* The conversation is the only scrollable region; keep its overscroll
               from chaining out to the (locked) document. */
            .fx-scroll {
              overscroll-behavior: contain;
              -webkit-overflow-scrolling: touch;
            }
            /* When the keyboard is up, drop the home-indicator inset (the keyboard
               already covers it) and hide the disclaimer so the input sits flush. */
            html[data-keyboard-open] .fx-input-dock {
              padding-bottom: 0.5rem !important;
            }
            html[data-keyboard-open] .fx-disclaimer {
              display: none !important;
            }
          <\\/style>
        </head>
        <body>
          <div id="root"></div>
          <script>
            var __fxCfg = \${JSON.stringify(sanitizeCfgForIframe(cfg))};
            try {
              // Same-origin srcdoc: hand the chat the host's LIVE config object so
              // functions survive — extraData/token factories and client-tool
              // handlers all work in place (no JSON copy, no handler-strip/bridge).
              // Cross-origin embeds throw on parent access and fall back to the
              // serialized snapshot + FlexieBridge path.
              window.FlexieAiConfig = (window.parent && window.parent !== window && window.parent.FlexieAiConfig)
                ? window.parent.FlexieAiConfig
                : __fxCfg;
            } catch (e) {
              window.FlexieAiConfig = __fxCfg;
            }
          <\\/script>
          <script type="module" src="\${ASSET_BASE + JS_FILE}"><\\/script>
          <script>
            // Iframe-side bridge — counterpart to the parent's full bridge.
            // Must expose both 'on' (receive parent → iframe messages) AND
            // 'send' (forward iframe → parent messages); without 'send', the
            // chat dispatcher cannot ask the parent to invoke a client tool.
            // Source-match validation: only accept messages from window.parent
            // so arbitrary same-page windows (extensions, ads, debug overlays)
            // cannot spoof bridge replies.
            window.FlexieBridge = window.FlexieBridge || (function(){
              return {
                on: function(type, fn) {
                  window.addEventListener('message', function(e) {
                    if (e.source !== window.parent) return;
                    var d = e.data;
                    if (!d || !d.__flexie__ || d.type !== type) return;
                    fn(d.payload);
                  });
                },
                send: function(type, payload) {
                  window.parent.postMessage({ __flexie__: true, type: type, payload: payload }, '*');
                }
              };
            })();
            
            window.addEventListener('DOMContentLoaded', () => {
              const styleTag = document.getElementById('parent-responsive-style');

              const applyViewport = ({ width }) => {
                const html = document.documentElement;
                if (width < 768) html.setAttribute('data-parent-mobile', '1');
                else html.removeAttribute('data-parent-mobile');

                if (styleTag) {
                  styleTag.textContent = width < 768
                    ? '.expand-separator, .expand-btn { display: none !important; }'
                    : '';
                }
              };

              window.FlexieBridge.on('viewport', applyViewport);

              // Parent reports the keyboard height in px (it owns the top-level
              // visualViewport). Expose it as a CSS var the app sizes against, and
              // flag the open state for the flush-input rules.
              var fxKbOpen = false;
              window.FlexieBridge.on('keyboard', (p) => {
                const html = document.documentElement;
                // The PARENT measured window.visualViewport.height (the iframe's own
                // visualViewport doesn't report the keyboard); size the chat UI to it.
                if (p && p.vh) html.style.setProperty('--fx-vh', p.vh + 'px');
                else html.style.removeProperty('--fx-vh');
                var open = !!(p && p.open);
                if (open) html.setAttribute('data-keyboard-open', '1');
                else html.removeAttribute('data-keyboard-open');
                // On the keyboard OPENING (rising edge) the conversation shrinks to the
                // band above the keys — jump it to the newest message so the user isn't
                // stranded mid-history. The chat app listens for this and reuses its
                // scroll-to-bottom.
                if (open && !fxKbOpen) {
                  window.dispatchEvent(new CustomEvent('fx-keyboard-open'));
                }
                fxKbOpen = open;
              });

              // THE active scroll-lock for the keyboard-open case: with the iframe
              // covering the screen, the finger lands on THIS document, not the host,
              // so the host's lock never sees it — this handler is what actually keeps
              // everything still. CAPTURE phase (+ passive:false) so it fires before any
              // inner scroller that might stopPropagation. Allow only the conversation
              // (.fx-scroll) and the textarea to move; preventDefault everything else.
              document.addEventListener('touchmove', function (e) {
                var t = e.target;
                if (t && t.closest && (t.closest('.fx-scroll') || t.closest('textarea, input, [contenteditable]'))) return;
                e.stopPropagation();
                e.preventDefault();
              }, { passive: false, capture: true });

              // Ping the parent the instant a field gains/loses focus so it can
              // re-measure the visual viewport across the keyboard animation (the
              // parent owns the only visualViewport that reports the keyboard).
              var fxPing = function (type) {
                return function (e) {
                  var t = e.target;
                  if (t && t.matches && t.matches('textarea, input, [contenteditable]')) {
                    window.FlexieBridge.send(type, {});
                  }
                };
              };
              document.addEventListener('focusin', fxPing('inputfocus'));
              document.addEventListener('focusout', fxPing('inputblur'));
            });
          <\\/script>
        </body>
      </html>\`;

    return iframe;
  }

  // ---------- Inline render mode (no iframe) --------------------
  // Mounts the chat as a real part of THIS document instead of inside a
  // style-isolated iframe. Used by a dedicated full-page chat / PWA: because
  // the chat is now a normal top-level document, native keyboard reflow,
  // 100dvh and iOS scroll-into-view all work on their own — none of the
  // iframe geometry/host-lock/visualViewport machinery is needed or run.
  // Client tools invoke in-place (same window), so no FlexieBridge round-trip.
  function mountInline(cfg) {
    // 1. Load the chat's stylesheet into the host page (once).
    if (CSS_FILE && !document.getElementById('flexie-chat-css')) {
      const link = document.createElement('link');
      link.id = 'flexie-chat-css';
      link.rel = 'stylesheet';
      link.href = ASSET_BASE + CSS_FILE;
      document.head.appendChild(link);
    }

    // 2. The bundle hard-mounts into #root (createRoot(getElementById('root'))).
    //    Reuse a #root the page already provides; otherwise create one inside
    //    the configured container (selector), defaulting to <body>.
    const container = (cfg.selector && document.querySelector(cfg.selector))
      || document.body || document.documentElement;
    let root = document.getElementById('root');
    if (!root) {
      root = document.createElement('div');
      root.id = 'root';
      container.appendChild(root);
    }
    root.classList.add('flexie-chat-inline'); // styling hook for the host page

    // 3. Visibility. On a dedicated page you set autoOpen:true (or omit it) and
    //    the chat is just there. The optional toggle button and the chat's own
    //    close button still work — inline mode has no iframe to slide, so we
    //    simply show/hide the mount node.
    let isOpen = cfg.autoOpen !== false;
    const applyOpen = () => { root.style.display = isOpen ? '' : 'none'; };
    applyOpen();

    // Same globals the chat UI calls (App.tsx → window.toggleFlexie*). The close
    // button calls toggleFlexieAiAgent — honor a host-supplied cfg.onClose (e.g. go
    // back so the page dismisses like a modal); otherwise default to show/hide #root.
    // Expand has no meaning on a full page, so it's an inert no-op rather than undefined.
    window.toggleFlexieAiAgent = (typeof cfg.onClose === 'function')
      ? cfg.onClose
      : () => { isOpen = !isOpen; applyOpen(); };
    window.toggleFlexieExpand = window.toggleFlexieExpand || function () {};

    if (cfg.toggleButtonSelector) {
      const btn = document.querySelector(cfg.toggleButtonSelector);
      if (btn) btn.addEventListener('click', () => window.toggleFlexieAiAgent());
    }

    // 4. Load the chat bundle into this document. It reads window.FlexieAiConfig
    //    (already present here) and mounts into #root. No srcdoc, no bridge.
    const script = document.createElement('script');
    script.type = 'module';
    script.src = ASSET_BASE + JS_FILE;
    document.body.appendChild(script);
  }
  // --------------------------------------------------------------

  function initChat() {
    const cfg = window.FlexieAiConfig || {};
    const container = cfg.selector ? document.querySelector(cfg.selector) : null;
    const iframe = createIframe(cfg, container);

    // The full-screen backdrop is created LAZILY the first time the chat is opened on
    // mobile (see ensureLockChrome), NEVER on load — on load we add nothing to the host
    // page. It sits behind the iframe and occludes the host while the chat is open.
    let backdrop = null;

    // A CLOSED chat must occupy NO space. The iframe is position:fixed, but pushed
    // off-screen via translateY(110%) with the tall mobile height glueIframe leaves on
    // it, it can still extend the host page's scrollable area — showing as a big empty
    // chunk at the bottom. So fully remove it from layout with display:none when closed.
    const showIframe = () => {
      // 'block', never '' — an iframe's default display is 'inline', which sits on the
      // text baseline and adds whitespace BELOW it that grows the host page's height.
      iframe.style.display = 'block';
      void iframe.offsetHeight; // reflow so the open slide animates from translateY(110%)
    };
    // Hide the moment the CLOSE slide actually finishes — driven by the transition's own
    // end event (no timer guessing the duration): when the open/close transform
    // transition ends and the chat is closed, drop the iframe from layout. If the user
    // re-opens mid-slide, isOpen is true here so it stays visible.
    iframe.addEventListener('transitionend', (e) => {
      if (e.propertyName === 'transform' && !isOpen) iframe.style.display = 'none';
    });

    iframe.addEventListener('load', () => {
      const sendViewport = () => {
        window.FlexieBridge?.send('viewport', {
          width: window.innerWidth,
          height: window.innerHeight
        });
      };

      sendViewport();
      window.addEventListener('resize', sendViewport);
    });

    // Store original collapsed dimensions and position
    const originalWidth = iframe.style.width;
    const originalHeight = iframe.style.height;
    const originalMaxHeight = iframe.style.maxHeight;
    const originalBottom = iframe.style.bottom;
    const originalRight = iframe.style.right;
    const originalBorderRadius = iframe.style.borderRadius;

    // Set transform origin to bottom right for expansion
    iframe.style.transformOrigin = 'bottom right';

    // Keep track of the state — restored from the last session so a page refresh
    // reopens the chat in its last size (full vs compact).
    let expandedState = false;
    try { expandedState = localStorage.getItem('fx_chat_expanded') === '1'; } catch (e) {}

    // ── Mobile keyboard-aware geometry ────────────────────────────────────
    // On phones, when the keyboard opens the space above it shrinks, so we size the
    // iframe to exactly that space (visualViewport.height) — the chat content then
    // fits the visible area with nothing hidden behind the keyboard for iOS to scroll
    // the iframe to. The HOST PAGE is hard-locked (see lockHostScroll) so it can't
    // scroll/pan behind the iframe either, and the keyboard covers the strip below
    // the iframe so no host shows. Net result: nothing scrolls but the messages.
    const MOBILE_BP = 768;
    // Mirror the CSS media query (max-width 768px) exactly via matchMedia, so the JS
    // (full-screen geometry + host lock) and the CSS (full-screen iframe) always
    // agree on the breakpoint — no innerWidth less-than vs CSS max-width off-by-one
    // at 768px, and it reads a live media-query match instead of polling innerWidth.
    const mobileMq = window.matchMedia('(max-width: ' + MOBILE_BP + 'px)');
    const isMobile = () => mobileMq.matches;
    let isOpen = cfg.autoOpen !== false;
    let geomRaf = false;

    // Scroll-lock on the HOST page, kept NON-INVASIVE to the host's layout. The lock
    // must NOT take the host <body> out of flow: the host app runs dynamic height
    // calculations everywhere, and position:fixed on body collapses the document and
    // feeds them wrong numbers (an empty chunk at the bottom). So we lock scrolling
    // WITHOUT moving anything:
    //  (1) capture-phase touchmove + wheel preventDefault on the document — cancels
    //      scrolling (finger AND mouse/trackpad) on EVERY host element (body, inner
    //      overflow:auto wrappers, AND datagrids/virtual scrollers that stopPropagation)
    //      with NO reflow. Capture is what reaches the grids; a bubble listener can't.
    //  (2) overflow:hidden on html+body — blocks the remaining (scrollbar) path. It
    //      does NOT change element sizes or take anything out of flow, so the host's
    //      height calcs are unaffected and the scroll position is preserved (no jump).
    // No position:fixed (it broke host layout), no recursive overflow:hidden-on-* (it
    // reflowed/shifted). iOS never scrolls the host to the input because the input
    // lives in the iframe; the conversation scrolls there, a separate document.
    // CAPTURE phase (+ passive:false) is essential: it fires document-first, BEFORE
    // any datagrid / virtual-scroller handler deeper in the tree. Those grids call
    // stopPropagation, so a bubble-phase listener never sees the event and the grid
    // scrolls. From capture we preventDefault (cancels native scroll) and
    // stopPropagation (neutralises JS-driven scrollers) before they run. We block
    // BOTH scroll-input paths: touchmove (finger, real devices) AND wheel
    // (mouse/trackpad — what fires in desktop device-emulation). No CSS, so no reflow.
    const LOCK_OPTS = { passive: false, capture: true };
    const blockHostScroll = (e) => {
      if (e.touches && e.touches.length > 1) return; // allow pinch-zoom
      e.stopPropagation();
      e.preventDefault();
    };
    const hostLockStyle = document.createElement('style');
    // overflow:hidden only — no position:fixed (it took body out of flow and broke the
    // host's height calcs), no recursive overflow:hidden-on-* (it reflowed/shifted).
    hostLockStyle.textContent =
      'html.fx-chat-locked, html.fx-chat-locked body {' +
        'overflow: hidden !important;' +
        'overscroll-behavior: none !important;' +
      '}';
    // Inject the lock stylesheet + build the backdrop the FIRST time the chat is opened
    // on mobile, never on load. Until the user opens the chat the host page is untouched.
    let lockChromeReady = false;
    const ensureLockChrome = () => {
      if (lockChromeReady) return;
      lockChromeReady = true;
      (document.head || document.documentElement).appendChild(hostLockStyle);
      backdrop = document.createElement('div');
      backdrop.className = 'flexie-chat-backdrop';
      backdrop.style.display = 'none';
      (container || document.body || document.documentElement).appendChild(backdrop);
    };
    const lockHostScroll = (lock) => {
      const de = document.documentElement;
      if (lock) {
        ensureLockChrome();
        de.classList.add('fx-chat-locked');
        document.addEventListener('touchmove', blockHostScroll, LOCK_OPTS);
        document.addEventListener('wheel', blockHostScroll, LOCK_OPTS);
        backdrop.style.display = 'block'; // occlude the host behind the chat (mobile)
      } else {
        de.classList.remove('fx-chat-locked');
        document.removeEventListener('touchmove', blockHostScroll, LOCK_OPTS);
        document.removeEventListener('wheel', blockHostScroll, LOCK_OPTS);
        if (backdrop) backdrop.style.display = 'none';
      }
    };

    // Single source of truth for the host lock: locked iff (mobile AND chat open).
    // Called from the toggle, init, and every viewport change, so crossing to a
    // desktop width (or closing) always unlocks. Idempotent via the hostLocked flag.
    let hostLocked = false;
    const syncHostLock = () => {
      const want = isMobile() && isOpen;
      if (want === hostLocked) return;
      hostLocked = want;
      lockHostScroll(want);
    };

    // Tell the iframe whether the keyboard is up (>120px of lost viewport rules out a
    // URL-bar shrink) so it can drop the home-indicator inset + disclaimer and sit
    // flush above the keys.
    const KB_THRESHOLD = 120;
    const sendKeyboardState = () => {
      const vv = window.visualViewport;
      const mobileOpen = isMobile() && isOpen && !!vv;
      // vh = window.visualViewport.height — what's ACTUALLY visible on screen (above
      // the keyboard). The chat UI sizes to this. 0 = clear (desktop → 100%).
      const vh = mobileOpen ? Math.round(vv.height) : 0;
      const open = mobileOpen && (window.innerHeight - vv.height > KB_THRESHOLD);
      if (window.FlexieBridge) window.FlexieBridge.send('keyboard', { vh: vh, open: open });
    };

    // Size + position the iframe ELEMENT onto the visual viewport (the rectangle above
    // the keyboard). Sizing the iframe to the visual viewport — NOT full screen — is
    // what keeps iOS from scrolling the focused textarea to the top. The backdrop behind
    // covers the host, so any lag/gap here just shows the backdrop, never the host.
    // Runs ONLY when the chat is open on mobile — on load (closed) we touch nothing.
    function glueIframe() {
      if (!isMobile() || !isOpen) return;
      var vv = window.visualViewport;
      if (!vv) return;
      iframe.style.top = Math.round(vv.offsetTop) + 'px';
      iframe.style.left = Math.round(vv.offsetLeft) + 'px';
      iframe.style.width = Math.round(vv.width) + 'px';
      iframe.style.height = Math.round(vv.height) + 'px';
      iframe.style.bottom = 'auto';
      iframe.style.right = 'auto';
    }

    // animate=true → open/close slide. animate=false → keyboard/resize tracking.
    function relayout(animate) {
      if (isMobile()) {
        // Geometry = visual viewport (glueIframe). Here we only drive the open/close
        // slide; the chat UI inside is clamped to the same height via --fx-vh.
        glueIframe();
        iframe.style.transition = animate
          ? 'transform 0.26s cubic-bezier(0.16, 1, 0.3, 1), opacity 0.2s ease-out'
          : 'none';
        iframe.style.transform = isOpen ? 'translateY(0)' : 'translateY(110%)';
        iframe.style.opacity = isOpen ? '1' : '0';
      } else {
        // Desktop keeps its inline 'transition: all 0.3s' for the open/close slide.
        // Open/close (transform + opacity) must work even while expanded — only the
        // height/position belong to the expand control, so leave those untouched
        // when expanded, and restore the inline default otherwise (e.g. after the
        // window is dragged back up from a mobile width).
        if (!expandedState) {
          iframe.style.height = originalHeight;
          iframe.style.maxHeight = originalMaxHeight;
          // Undo the mobile JS-driven geometry (in case we crossed back from a
          // phone width) so the floating frame docks bottom-right again.
          iframe.style.top = '';
          iframe.style.left = '';
          iframe.style.width = originalWidth;
          iframe.style.right = originalRight;
          iframe.style.bottom = originalBottom;
        }
        iframe.style.transform = isOpen
          ? (expandedState ? 'scale(1)' : 'translateY(0)')
          : 'translateY(110%)';
        iframe.style.opacity = isOpen ? '1' : '0';
      }
    }

    const onViewportChange = () => {
      syncHostLock();
      glueIframe(); // immediate — keep the iframe on the visual viewport with no lag
      if (!isMobile() || !isOpen) { relayout(false); sendKeyboardState(); return; }
      if (geomRaf) return;
      geomRaf = true;
      requestAnimationFrame(() => { geomRaf = false; relayout(false); sendKeyboardState(); });
    };

    if (window.visualViewport) {
      // 'resize' = keyboard open/close (height changes) → relayout + host lock.
      // 'scroll' = visual-viewport PAN (offsetTop changes on iOS focus) → re-glue the
      // iframe onto the moved visible rectangle. The backdrop behind covers the host,
      // so a lag frame just shows the backdrop, never host.
      window.visualViewport.addEventListener('resize', onViewportChange);
      window.visualViewport.addEventListener('scroll', glueIframe);
    }
    window.addEventListener('resize', onViewportChange);

    // The iframe pings us the instant its textarea gains/loses focus. iOS animates
    // the keyboard over ~300ms and visualViewport 'resize' can fire late or coalesce,
    // so re-measure on a short burst to lock onto the final visible rectangle.
    const burstRelayout = () => {
      [0, 100, 200, 350, 550].forEach((ms) => setTimeout(() => {
        relayout(false); sendKeyboardState();
      }, ms));
    };
    if (window.FlexieBridge) {
      window.FlexieBridge.on('inputfocus', burstRelayout);
      window.FlexieBridge.on('inputblur', burstRelayout);
    }

    window.FlexieIframe = iframe;
    // Apply the current expandedState to the iframe SIZE/position only (transform is
    // owned by relayout). On mobile the chat is ALWAYS full-screen (glueIframe + the
    // responsive CSS own the geometry), so the persisted desktop expand/compact state
    // is ignored there entirely. Body scroll is only locked while the panel is open,
    // so a restored "expanded" flag never locks the host page before it is opened.
    const applyExpandGeometry = () => {
      if (isMobile()) return;
      if (expandedState) {
        // Expand to full screen from bottom right
        iframe.style.width = '100vw';
        iframe.style.height = '100vh';
        iframe.style.maxHeight = 'inherit';
        iframe.style.bottom = '0';
        iframe.style.right = '0';
        iframe.style.borderRadius = '0';
        if (isOpen) document.body.style.overflow = 'hidden';
      } else {
        // Collapse back to original position and size
        iframe.style.width = originalWidth;
        iframe.style.height = originalHeight;
        iframe.style.maxHeight = originalMaxHeight;
        iframe.style.bottom = originalBottom;
        iframe.style.right = originalRight;
        iframe.style.borderRadius = originalBorderRadius;
        document.body.style.overflow = '';
      }
    };

    window.toggleFlexieExpand = () => {
      expandedState = !expandedState;
      // Remember the size across refreshes so reopening restores it.
      try { localStorage.setItem('fx_chat_expanded', expandedState ? '1' : '0'); } catch (e) {}
      applyExpandGeometry();
    };

    window.toggleFlexieAiAgent = () => {
      isOpen = !isOpen;
      if (isOpen) {
        showIframe();             // display:block before showing
        applyExpandGeometry();    // restore the last size (desktop only; mobile stays full)
      } else {
        document.body.style.overflow = ''; // never leave the host page scroll-locked
      }
      syncHostLock();
      relayout(false);            // instant show/hide, no slide animation
      sendKeyboardState();
      // No transition to wait on, so hide immediately on close (the transitionend
      // hook never fires without a running transition).
      if (!isOpen) iframe.style.display = 'none';
    };

    // Floating toggle (only applies if fixed)
    if (cfg.toggleButtonSelector) {
      const btn = document.querySelector(cfg.toggleButtonSelector);
      if (btn) {
        btn.addEventListener('click', () => {
          window.toggleFlexieAiAgent();
        });
      }
    }

    // Normalize initial geometry — on mobile the height is now JS-driven (no
    // longer set by CSS), so size the iframe to the visual viewport up front.
    relayout(false);
    // Restore the last size on load: if the chat auto-opens expanded, apply the
    // full-screen geometry up front (relayout above already respects expandedState).
    applyExpandGeometry();
    syncHostLock();
    sendKeyboardState();
    // Closed on load → take up no space at all; open → visible (block, not the iframe
    // default 'inline' which adds baseline whitespace that grows the host page).
    iframe.style.display = isOpen ? 'block' : 'none';
  }

  // Route by render mode: 'inline' mounts the chat straight into this document
  // (dedicated page / PWA); anything else keeps the floating iframe widget.
  function boot(cfg) {
    if (cfg && cfg.renderMode === 'inline') return mountInline(cfg);
    return initChat();
  }

  window.FlexieAiEmbedChat = { init: boot };
  window.FlexieAiEmbedChat.updateConfig = function (newCfg) {
    // Iframe mode relays into the frame; inline mode lives in THIS window, so
    // post to ourselves — App.tsx listens for 'configUpdate' on window in both.
    const target = window.FlexieIframe?.contentWindow || window;
    target.postMessage(
      { __flexie__: true, type: 'configUpdate', payload: newCfg },
      '*'
    );
  };

  (function waitForReady() {
    const cfg = window.FlexieAiConfig;
    if (cfg) {
      boot(cfg);
      return;
    }

    setTimeout(waitForReady, 100);
  })();
})();
`

fs.writeFileSync(path.join(dist, 'ai-embed.js'), embedCode)
console.log(`✅ Created ${process.env.SAG_OUT_DIR || 'dist/app'}/ai-embed.js (universal embed version)`)
