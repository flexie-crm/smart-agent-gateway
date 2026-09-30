/// <reference types="vite/client" />

interface ImportMetaEnv {
  /**
   * Origin of the gateway's API. Set in development, where the console and
   * the gateway run on different ports; empty in production, where they
   * share an origin.
   */
  readonly VITE_SAG_API_URL?: string
  /**
   * Whether this build runs models on the computer it is installed on. "0" on a
   * platform we ship no engine for, unset everywhere else. See LOCAL_ENGINE.
   */
  readonly VITE_SAG_LOCAL_ENGINE?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
