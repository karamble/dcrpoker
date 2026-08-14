import { configure } from './api'

// The token this page was opened with.
//
// It arrives in the fragment of the URL the game printed at startup, and it
// STAYS there: the page has to survive a reload, a bookmark and a restored tab,
// and a credential held anywhere but the address bar cannot. A fragment is
// never sent to a server and never lands in a log, which is why it is the
// fragment and not a query.
//
// sessionStorage is only a fallback, for a URL that arrived without one. It
// dies with the tab and the token dies with the process that minted it, so a
// stale one can only ever fail closed.

const tokenKey = 'pokerplugin.token'

// Storage throws when it is disabled, and a page that cannot remember a token
// must still work from a fresh link rather than fail to start.
function remember(token: string): void {
  try {
    window.sessionStorage.setItem(tokenKey, token)
  } catch {
    /* Nothing to do: the link still configured this page. */
  }
}

function remembered(): string {
  try {
    return window.sessionStorage.getItem(tokenKey) ?? ''
  } catch {
    return ''
  }
}

/** forgetToken drops a token the plugin no longer accepts, so a reload asks for
 *  a fresh link instead of retrying one that cannot work. */
export function forgetToken(): void {
  try {
    window.sessionStorage.removeItem(tokenKey)
  } catch {
    /* Nothing to do. */
  }
}

/** bootstrapSession settles the token before anything can ask with it.
 *
 *  Called from main.tsx before createRoot, so the first request already has it.
 *  Doing this during render is what once let the first poll go out
 *  unauthenticated, get a 404, and throw the stored token away. */
export function bootstrapSession(): void {
  const fragment = new URLSearchParams(window.location.hash.replace(/^#/, ''))
  const fromURL = fragment.get('token') ?? ''
  if (fromURL) remember(fromURL)
  // The document is served at /ui/, so the routes are one level up.
  configure({ token: fromURL || remembered(), apiBase: '..' })
}
