import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'
// @ts-expect-error a plain module, shared by both front ends
import { buildStamp } from '../scripts/build-stamp.mjs'

// Stamped into every build, automatically. See chat-ui/vite.config.js.
const STAMP: string = buildStamp()

// The console calls the orchestrator's API directly (VITE_SAG_API_URL in
// development, same origin in production). This server only serves the UI:
// there is no proxy in front of the API, in any environment.
export default defineConfig({
  define: { __SAG_BUILD__: JSON.stringify(STAMP) },
  plugins: [react()],
  resolve: {
    alias: { '@': path.resolve(__dirname, './src') },
  },
  build: {
    // dist by default, and somewhere else when a caller says so.
    //
    // The desktop build carries VITE_SAG_PERSONAL, which makes a DIFFERENT
    // product out of the same source. Writing it into dist would replace the
    // bundle a running gateway serves, silently, with one built for another
    // edition (see chat-ui/vite.config.js: it happened).
    outDir: process.env.SAG_OUT_DIR || 'dist',
  },
  server: {
    // Listen on every address family. Bound to IPv4 loopback only, a browser
    // that resolves localhost to ::1 first (Windows does) burns its whole
    // IPv6 fallback delay, ~300ms, on every connection before retrying over
    // IPv4. That tax dwarfs the request itself.
    host: true,
    port: 5174,
  },
  test: {
    environment: 'jsdom',
    setupFiles: './src/test-setup.ts',
  },
})
