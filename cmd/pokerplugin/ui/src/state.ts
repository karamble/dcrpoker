import { useEffect, useReducer, useRef } from 'react'
import {
  api,
  authToken,
  haveToken,
  streamPath,
  type HandView,
  type LedgerView,
  type Snapshot,
} from './api'

// One picture of the table, from either of two transports.
//
// The plugin's stream sends exactly the bodies its routes answer with, so
// pushing and asking produce identical values and there is one reducer for
// both. That is not a tidiness argument: the fallback path is the one that runs
// when something is already wrong, and a second code path there would be the
// least exercised and most important code in the file.
//
// The stream is read with fetch rather than EventSource, because EventSource
// cannot set a header and the only alternative is the token in the URL - where
// it lands in history, in referrers and in every log between here and the
// plugin. A short-lived token is still a token.

export type State = {
  tables: Snapshot[]
  hands: Record<string, HandView>
  ledgers: Record<string, LedgerView>
  /** live says the stream is connected. When it is false everything below is
   *  still true, just possibly a second or two old - which is worth saying
   *  rather than hiding, because "nothing is happening" and "we stopped
   *  hearing" look identical and are not. */
  live: boolean
  error?: string
}

type Action =
  | { type: 'tables'; tables: Snapshot[] }
  | { type: 'hand'; sid: string; view: HandView }
  | { type: 'ledger'; sid: string; view: LedgerView }
  | { type: 'live'; live: boolean }
  | { type: 'error'; error?: string }

const empty: State = { tables: [], hands: {}, ledgers: {}, live: false }

/** ordered fixes the order tables are shown in, newest first and finished ones
 *  last.
 *
 *  The plugin already sorts, and this repeats it rather than trusting it. The
 *  cost of being wrong is not cosmetic: these are rendered as a row of buttons
 *  somebody is about to press, and a list that reorders under a cursor is a list
 *  that gets the wrong table picked. Sorting twice is a few microseconds against
 *  a mis-click on a table holding money. */
function ordered(tables: Snapshot[]): Snapshot[] {
  return [...tables].sort((a, b) => {
    if (!!a.finished !== !!b.finished) return a.finished ? 1 : -1
    if ((a.until ?? 0) !== (b.until ?? 0)) return (b.until ?? 0) - (a.until ?? 0)
    return a.sid < b.sid ? -1 : a.sid > b.sid ? 1 : 0
  })
}

function reduce(state: State, action: Action): State {
  switch (action.type) {
    case 'tables': {
      // Drop what we know about tables that are gone, so a stale hand cannot
      // outlive the table it belonged to.
      const live = new Set(action.tables.map((t) => t.sid))
      const hands: Record<string, HandView> = {}
      const ledgers: Record<string, LedgerView> = {}
      for (const [sid, v] of Object.entries(state.hands)) if (live.has(sid)) hands[sid] = v
      for (const [sid, v] of Object.entries(state.ledgers)) if (live.has(sid)) ledgers[sid] = v
      return { ...state, tables: ordered(action.tables), hands, ledgers }
    }
    case 'hand':
      return { ...state, hands: { ...state.hands, [action.sid]: action.view } }
    case 'ledger':
      return { ...state, ledgers: { ...state.ledgers, [action.sid]: action.view } }
    case 'live':
      return { ...state, live: action.live }
    case 'error':
      return { ...state, error: action.error }
  }
}

/** pollEvery is how often the fallback asks. Slower than the stream on
 *  purpose: it runs when the stream is down, and hammering a plugin that is
 *  already struggling is not help. */
const pollEvery = 2000

export function useTableState(ready: boolean): State {
  const [state, dispatch] = useReducer(reduce, empty)
  const stopped = useRef(false)

  useEffect(() => {
    if (!ready) return
    stopped.current = false
    const control = new AbortController()

    // The fallback. It runs whenever the stream is not connected, and it feeds
    // the same reducer with the same shapes.
    let polling: number | undefined
    const pollOnce = async () => {
      try {
        const { tables } = await api.tables()
        dispatch({ type: 'tables', tables })
        for (const t of tables) {
          try {
            dispatch({ type: 'ledger', sid: t.sid, view: await api.ledger(t.sid) })
          } catch {
            // A ledger that cannot be read is not a reason to stop
            // reading the others.
          }
          if (!t.dealing) continue
          try {
            dispatch({ type: 'hand', sid: t.sid, view: await api.hand(t.sid) })
          } catch {
            // Between hands the plugin answers 400, which is a state
            // and not a failure.
          }
        }
        dispatch({ type: 'error', error: undefined })
      } catch (err) {
        dispatch({ type: 'error', error: String(err instanceof Error ? err.message : err) })
      }
    }
    const startPolling = () => {
      if (polling !== undefined) return
      void pollOnce()
      polling = window.setInterval(() => void pollOnce(), pollEvery)
    }
    const stopPolling = () => {
      if (polling === undefined) return
      window.clearInterval(polling)
      polling = undefined
    }

    const apply = (event: string, data: string) => {
      try {
        const body = JSON.parse(data)
        if (event === 'state') dispatch({ type: 'tables', tables: body.tables ?? [] })
        else if (event === 'hand') dispatch({ type: 'hand', sid: body.sid, view: body.view })
        else if (event === 'ledger') dispatch({ type: 'ledger', sid: body.sid, view: body.view })
      } catch {
        // A frame that will not parse is one frame. The next one carries
        // whole state, so there is nothing to recover.
      }
    }

    const connect = async (backoff: number) => {
      if (stopped.current) return
      try {
        const res = await fetch(streamPath(), {
          headers: { Authorization: `Bearer ${authToken()}` },
          signal: control.signal,
        })
        if (!res.ok || !res.body) throw new Error(`the stream answered ${res.status}`)

        stopPolling()
        dispatch({ type: 'live', live: true })
        dispatch({ type: 'error', error: undefined })

        const reader = res.body.getReader()
        const decoder = new TextDecoder()
        let buffer = ''
        for (;;) {
          const { done, value } = await reader.read()
          if (done) break
          buffer += decoder.decode(value, { stream: true })
          // Frames are separated by a blank line. A partial one stays in
          // the buffer until the rest arrives.
          let cut: number
          while ((cut = buffer.indexOf('\n\n')) !== -1) {
            const frame = buffer.slice(0, cut)
            buffer = buffer.slice(cut + 2)
            let event = ''
            let data = ''
            for (const line of frame.split('\n')) {
              if (line.startsWith('event: ')) event = line.slice(7)
              else if (line.startsWith('data: ')) data += line.slice(6)
            }
            if (event && data) apply(event, data)
          }
        }
        throw new Error('the stream ended')
      } catch (err) {
        if (stopped.current || control.signal.aborted) return
        dispatch({ type: 'live', live: false })
        // Ask while we are not being told. The fallback is not a
        // degraded mode to warn about; it is what keeps the table
        // readable while the stream comes back.
        startPolling()
        const next = Math.min(backoff * 2, 30000)
        window.setTimeout(() => void connect(next), backoff)
        void err
      }
    }

    startPolling()
    void connect(1000)

    return () => {
      stopped.current = true
      control.abort()
      stopPolling()
    }
  }, [ready])

  return state
}

export { haveToken }
