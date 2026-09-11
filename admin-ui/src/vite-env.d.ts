/// <reference types="vite/client" />

interface ImportMetaEnv {
  /**
   * Origin of the gateway's API. Set in development, where the console and
   * the gateway run on different ports; empty in production, where they
   * share an origin.
   */
  readonly VITE_SAG_API_URL?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
