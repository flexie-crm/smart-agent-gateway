import { useEffect, useState } from 'react'

import { fetchServerIsDev } from '@lib/api'

/**
 * Whether the server answering this page is a working copy.
 *
 * Asked once and shared by everything that needs it: the sign-in screen, which
 * offers a button because of it, and the sidebar, which says so where somebody
 * will see it. It starts false, so the badge can only ever appear because a
 * server said it should.
 */
export function useServerIsDev(): boolean {
  const [dev, setDev] = useState(false)

  useEffect(() => {
    let live = true
    void fetchServerIsDev().then((isDev) => {
      if (live) setDev(isDev)
    })
    return () => {
      live = false
    }
  }, [])

  return dev
}
