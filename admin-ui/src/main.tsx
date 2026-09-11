import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

import { askTheApplication, paint, rememberedChoice } from '@/lib/theme'
import './index.css'
import { POSTURE } from '@/lib/api'
import { makeItFeelNative } from '@/lib/desktop-chrome'

if (POSTURE.local_sign_in) makeItFeelNative()
import { App } from './App'

// What this build is, readable from the page and greppable in the file. See
// chat-ui/src/main.tsx.
declare const __SAG_BUILD__: string
;(window as unknown as { __SAG_BUILD__: string }).__SAG_BUILD__ = __SAG_BUILD__

// Before the first paint, or somebody working at night gets a white flash on
// every reload. See chat-ui/src/main.tsx: the same reason, the same place.
paint(rememberedChoice())

// And ask the application, which is the half that survives a gateway choosing a
// new port and taking this page's storage with it.
askTheApplication()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
