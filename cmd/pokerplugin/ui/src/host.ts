import { useEffect, useState } from 'react'
import { configure } from './api'

// The boundary, written as an API.
//
// This page runs in a frame with an opaque origin. It cannot read the
// dashboard, the dashboard cannot read it, and neither can name the other's
// origin in a postMessage - so the host opens a MessageChannel and hands one
// port over. Everything after that travels on the port.
//
// The list below is the whole of what crosses. It is deliberately tiny, and
// two things are deliberately absent:
//
//   - No address and no amount ever comes *from* this page. The payout address
//     is injected by the host, which derives it from the wallet account the
//     user bound to gaming. A page that could name where winnings go is a page
//     that could redirect them.
//   - No signature and no key. The plugin signs; this is a view and an input
//     device. Letting the UI pre-sign to save a round trip would put the key
//     that publishes itself on equivocation inside a sandboxed frame.

export type HostInit = {
  v: 1
  apiBase?: string
  token: string
  expiresAt?: string
  tableId?: string | null
  payoutAddress?: string
}

export type Host = {
  ready: boolean
  tableId?: string
  payoutAddress?: string
  /** close asks the host to take the panel away. The host owns the window; all
   *  this can do is say the person asked. */
  close: () => void
  title: (text: string) => void
}

/** standalone is a page opened directly at the plugin rather than through the
 *  host - which is how it is developed, and how somebody debugging a container
 *  reaches it. There is no port and no token, and the plugin's own guard is
 *  what decides whether the requests work. */
function standalone(): boolean {
  return window.parent === window
}

export function useHost(): Host {
  const [state, setState] = useState<Host>({
    ready: standalone(),
    close: () => {},
    title: () => {},
  })

  useEffect(() => {
    if (standalone()) {
      // The token is in the fragment of the URL the game printed when it
      // started. A fragment rather than a query because it is never sent to
      // a server and never lands in a log, and this page is the only thing
      // that reads it.
      //
      // It is taken out of the address bar immediately afterwards, so a
      // screenshot or a shoulder does not carry it, and so reloading from
      // history does not resurrect a token from a previous run.
      const fragment = new URLSearchParams(window.location.hash.slice(1))
      const fromURL = fragment.get('token') ?? ''
      if (fromURL) {
        window.history.replaceState(null, '', window.location.pathname)
      }
      configure({ token: fromURL, apiBase: '.' })
      return
    }

    let port: MessagePort | undefined

    const onPort = (ev: MessageEvent) => {
      // The first message carries a port and nothing else. It cannot carry
      // a secret, because its targetOrigin has to be '*' - an opaque origin
      // cannot be named - so anything in it is readable by anyone who can
      // get a frame onto the page.
      if (!ev.data || ev.data.type !== 'pulse.port' || !ev.ports?.length) return
      window.removeEventListener('message', onPort)
      port = ev.ports[0]
      port.onmessage = onMessage
      port.start()
      port.postMessage({ type: 'game.ready', v: 1 })
    }

    const onMessage = (ev: MessageEvent) => {
      const msg = ev.data
      if (!msg || msg.v !== 1) return
      switch (msg.type) {
        case 'pulse.init': {
          const init = msg as HostInit
          configure({ token: String(init.token ?? ''), apiBase: init.apiBase })
          setState((s) => ({
            ...s,
            ready: true,
            tableId: init.tableId ?? undefined,
            payoutAddress: init.payoutAddress,
          }))
          break
        }
        case 'pulse.token':
          // Rotated while the panel is open, so a hand never dies of a
          // token expiring mid-street.
          configure({ token: String(msg.token ?? '') })
          break
        case 'pulse.close':
          setState((s) => ({ ...s, ready: false }))
          break
      }
    }

    window.addEventListener('message', onPort)

    setState((s) => ({
      ...s,
      close: () => port?.postMessage({ type: 'game.close', v: 1 }),
      title: (text: string) =>
        port?.postMessage({ type: 'game.title', v: 1, text: String(text).slice(0, 200) }),
    }))

    return () => {
      window.removeEventListener('message', onPort)
      port?.close()
    }
  }, [])

  return state
}
