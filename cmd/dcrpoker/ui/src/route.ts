import { useSyncExternalStore } from 'react'

// The address bar is the state.
//
// Three fields travel in the fragment and they are read and written one at a
// time, never as a whole string. That is not tidiness: the token lives in the
// same fragment, it is the only credential this page has, and a write that
// replaced the fragment wholesale would sign the page out. `keeps` below is
// there because that is what went wrong every time it went wrong.
//
// A fragment is used rather than a path because the plugin serves one document
// at /ui/ and answers everything else with 404 by design - see ui.go. A path
// router would need a single-page fallback there, which would turn a missing
// asset into a blank page.

export type Tab = 'now' | 'money' | 'roster' | 'audit'

export type Route =
  | { view: 'lobby' }
  | { view: 'table'; sid: string; tab: Tab }
  | { view: 'felt'; sid: string }

const tabs: readonly Tab[] = ['now', 'money', 'roster', 'audit']

function fields(): URLSearchParams {
  return new URLSearchParams(window.location.hash.replace(/^#/, ''))
}

/** read is what the address bar currently says. An unknown or absent view is
 *  the lobby, so the URL the game prints - which carries a token and nothing
 *  else - lands somewhere real. */
export function read(): Route {
  const p = fields()
  const sid = p.get('sid') ?? ''
  const view = p.get('view')
  if (sid && view === 'felt') return { view: 'felt', sid }
  if (sid && view === 'table') {
    const asked = p.get('tab')
    return { view: 'table', sid, tab: tabs.find((t) => t === asked) ?? 'now' }
  }
  return { view: 'lobby' }
}

/** href is the fragment for a route, with every field this page did not name
 *  left exactly as it was. */
export function href(next: Route): string {
  const p = fields()
  p.set('view', next.view)
  if (next.view === 'lobby') {
    p.delete('sid')
    p.delete('tab')
  } else {
    p.set('sid', next.sid)
    if (next.view === 'table') p.set('tab', next.tab)
    else p.delete('tab')
  }
  return `#${p.toString()}`
}

/** keeps refuses a write that would drop the token.
 *
 *  A route bug you can see is a wrong screen. This one is not visible: the
 *  fragment loses the token, the next request is answered 404, and the page
 *  says it is signed out. Cheaper to make impossible than to find again. */
function keeps(after: string): boolean {
  const was = fields().get('token')
  return !was || new URLSearchParams(after.slice(1)).get('token') === was
}

function key(r: Route): string {
  if (r.view === 'lobby') return 'lobby'
  if (r.view === 'felt') return `felt:${r.sid}`
  return `table:${r.sid}:${r.tab}`
}

let current = read()
let currentKey = key(current)
const listeners = new Set<() => void>()

function refresh(): void {
  const next = read()
  const k = key(next)
  // Same route, same object. useSyncExternalStore compares by identity and
  // loops if this hands back a fresh object for an unchanged address.
  if (k === currentKey) return
  current = next
  currentKey = k
  for (const l of listeners) l()
}

// Back and forward. pushState and replaceState do not fire this, which is why
// go() calls refresh() itself.
window.addEventListener('hashchange', refresh)

function subscribe(l: () => void): () => void {
  listeners.add(l)
  return () => {
    listeners.delete(l)
  }
}

function snapshot(): Route {
  return current
}

export function useRoute(): Route {
  return useSyncExternalStore(subscribe, snapshot)
}

/** go moves. `replace` for anything the person did not ask for - the pull to
 *  the felt, the fall back to the lobby - so Back returns them to where they
 *  were rather than to where they were sent. */
export function go(next: Route, opts?: { replace?: boolean }): void {
  if (key(next) === currentKey) return
  const url = href(next)
  if (!keeps(url)) return
  if (opts?.replace) window.history.replaceState(null, '', url)
  else window.history.pushState(null, '', url)
  refresh()
}
