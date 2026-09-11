import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
import { visualizer } from 'rollup-plugin-visualizer'
import { buildStamp } from '../scripts/build-stamp.mjs'

// Stamped into every build, automatically, so that "which version is this?" is
// answerable about a file on disk, a page in a browser and a page inside an
// application, without anybody having to remember to pass anything.
const STAMP = buildStamp()

const mode = process.env.BUILD_MODE || 'app'
const isLib = mode === 'lib'

// SAG: the UI talks to the orchestrator DIRECTLY (VITE_SAG_API_URL), like the
// console. No dev proxy: vite only serves the React app.
export default defineConfig({
  define: { __SAG_BUILD__: JSON.stringify(STAMP) },
  // RELATIVE, so this build works wherever it is mounted and there is nothing to
  // configure. The gateway serves it under /chat/ when it serves both pages, and
  // at the root of its own host when a proxy puts it there; the same files do
  // both, because `./assets/...` resolves against the page that asked for it.
  //
  // It was an absolute base with an env var to override, and that is exactly how
  // this broke twice in one afternoon: `make ci` rebuilds the chat and does not
  // set the variable, so the gate whose job is to catch breakage silently
  // replaced a working build with one whose assets point at another
  // application's. The symptom is the one this comment always described: every
  // asset falls through the SPA fallback, comes back as index.html, and the
  // browser refuses it for having a MIME type of text/html. It reads as a broken
  // build, and it is not one; it is a build made for a different mount point.
  //
  // Relative is only safe because this app never changes its own path: there is
  // no router, nothing pushes history, nothing reads location.pathname. If that
  // stops being true, a deep URL would resolve assets one level too far down and
  // this has to become absolute again, set from where it is served.
  //
  // Still overridable, and one caller needs it: Vite's DEV SERVER serves from an
  // absolute base, so `make dev-personal` sets /chat/ for the page it hosts. A
  // BUILD never needs it, which is why the default is the relative one.
  //
  // The embed has an override of its own and does not use this.
  base: process.env.VITE_BASE_PATH || './',

  // SAG: the chat signs itself in now, so its tests need a DOM: the token
  // lives in localStorage. The CRM's chat never authenticated itself.
  test: {
    environment: 'jsdom',
    // The e2e/ specs are Playwright, not vitest: they drive a real browser via
    // `make e2e`. Excluded here so the unit run does not try to execute them.
    exclude: ['e2e/**', 'node_modules/**', 'dist/**'],
  },

  server: {
      // Listen on every address family. Bound to IPv4 only, a browser that
      // resolves localhost to ::1 first (Windows does) burns its whole IPv6
      // fallback delay, ~300ms, on every connection before retrying over
      // IPv4. That tax dwarfs the request itself.
      host: true,
      port: 5173,
    },
    plugins: [
    react(),
    // Bundle analysis only when not in CI
    process.env.ANALYZE === 'true' && visualizer({
      filename: 'dist/stats.html',
      open: false,
      gzipSize: true,
      brotliSize: true,
    }),
  ].filter(Boolean),

  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
      '@components': path.resolve(__dirname, './src/components'),
      '@lib': path.resolve(__dirname, './src/lib'),
    },
  },

  build: isLib
    ? {
        lib: {
          entry: path.resolve(__dirname, 'src/index.ts'),
          name: 'FlexieAiAgent',
          fileName: 'flexie-ai-agent',
          formats: ['es'],
        },
        rollupOptions: {
          external: [
            'react',
            'react-dom',
            'react/jsx-runtime',
            '@radix-ui/react-*',
            'lucide-react',
          ],
          output: {
            preserveModules: false,
            exports: 'named',
          },
        },
        outDir: 'dist/lib',
        target: 'esnext',
      }
    : {
        // dist/app by default, and somewhere else when a caller says so.
        //
        // A DESKTOP build is a different product from the web one (it carries
        // VITE_SAG_PERSONAL, which turns on the console link, the native window
        // behaviour and everything else that is only true inside the
        // application). Writing it here would replace the bundle a running
        // gateway serves with one built for a different edition, and nothing
        // would say so: the web chat simply starts showing a console link and
        // suppressing the right-click menu. That happened.
        outDir: process.env.SAG_OUT_DIR || 'dist/app',
        assetsDir: 'assets',
        sourcemap: false,
        minify: 'terser',
        terserOptions: {
          compress: {
            drop_console: true,
            drop_debugger: true,
          },
        },
        rollupOptions: {
          output: {
            format: 'es',
            entryFileNames: 'assets/index-[hash].js',
            chunkFileNames: 'assets/[name]-[hash].js',
            manualChunks(id) {
              // React core runtime
              if (id.includes('node_modules/react-dom') || id.includes('node_modules/react/') || id.includes('node_modules/scheduler')) {
                return 'react-vendor';
              }
              // A language grammar is fetched when a code block needs it, so
              // each one is its OWN chunk. Left to the default it lands in
              // syntax-hl with the highlighter, and then the first block of any
              // language pulls all twenty-three: a lazy import that loads
              // everything is what the eager imports already were.
              //
              // Naming it explicitly is what actually separates it. The grammars
              // are CommonJS, and the conversion hoists their factories into
              // whichever chunk Rollup feels like, which is how they ended up
              // back together the first time.
              const grammar = /(?:languages\/prism|refractor\/lang)\/([a-z0-9+#_-]+)\.js/.exec(id);
              if (grammar) {
                return `lang-${grammar[1]}`;
              }
              // The highlighter itself, which is heavy and worth its own chunk.
              if (id.includes('react-syntax-highlighter') || id.includes('refractor') || id.includes('prismjs')) {
                return 'syntax-hl';
              }
              // Markdown processing
              if (id.includes('react-markdown') || id.includes('remark-') || id.includes('rehype-') || id.includes('unified') || id.includes('mdast') || id.includes('hast') || id.includes('micromark') || id.includes('harden-react-markdown') || id.includes('devlop') || id.includes('property-information') || id.includes('space-separated-tokens') || id.includes('comma-separated-tokens') || id.includes('vfile') || id.includes('unist-util') || id.includes('decode-named-character-reference') || id.includes('ccount') || id.includes('bail') || id.includes('trough') || id.includes('is-plain-obj') || id.includes('trim-lines') || id.includes('character-entities') || id.includes('html-void-elements') || id.includes('stringify-entities') || id.includes('estree-util')) {
                return 'markdown';
              }
              // Radix UI
              if (id.includes('@radix-ui')) {
                return 'radix-ui';
              }
              // Icons
              if (id.includes('lucide-react')) {
                return 'icons';
              }
            },
          },
        },
      },
})
