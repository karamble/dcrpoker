import { useEffect, useRef } from 'react'
import { go, type Route } from './route'
import type { State } from './state'
import type { TableView } from './view'

// The four rules that decide when the screen moves on its own.
//
// This used to be one boolean - `table && !table.finished && (table.dealing ||
// holding)` - and a tab check. Written out it is four rules, and writing them
// out is the gain: this is the part of the behaviour a player notices when it
// is wrong, and an emergent policy cannot be read.
//
// Every redirect replaces rather than pushes. Nobody asked to be moved, so Back
// should return them to where they were rather than to where they were sent -
// and StrictMode runs effects twice in development, which a replace survives.

export function useRouteGuards(route: Route, view: TableView, state: State): void {
  // The tables that have already taken the screen. Edge-triggered, so somebody
  // who walks back to the lobby during a hand is not dragged forward again.
  const pulled = useRef(new Set<string>())

  useEffect(() => {
    // 1. The hold freezes everything. Those fifteen seconds exist so somebody
    //    can read what just happened, and a redirect through them is the one
    //    thing that could take it away.
    if (view.holding) return

    // 2. The pull. A table that begins dealing takes the screen, but only from
    //    the lobby: on a table view you chose that view, and Progress already
    //    offers the felt while it deals.
    //
    //    Which tables are dealing is recorded wherever we are, not only at the
    //    lobby. The pull is an edge, and an edge noticed only at the lobby
    //    would fire again the moment somebody walked back there from a table
    //    they had opened themselves.
    const live = new Set(state.tables.map((t) => t.sid))
    for (const sid of [...pulled.current]) if (!live.has(sid)) pulled.current.delete(sid)
    let started: string | undefined
    for (const t of state.tables) {
      // A table that has ended is not a table to be dragged to. What is left
      // of it is the payout, and that is a record rather than a hand.
      if (!t.dealing || t.over || t.finished) continue
      if (pulled.current.has(t.sid)) continue
      pulled.current.add(t.sid)
      if (started === undefined) started = t.sid
    }

    if (route.view === 'lobby') {
      if (started) go({ view: 'felt', sid: started }, { replace: true })
      return
    }

    // 3. A sid nothing matches. Not resolved to some other table: showing a
    //    different table than the address bar names is the failure worth
    //    avoiding on a page about money. `loaded` is what makes this safe to
    //    apply - before the first answer, an unknown sid is only unknown yet.
    if (!view.table) {
      if (state.loaded) go({ view: 'lobby' }, { replace: true })
      return
    }

    if (route.view === 'felt') {
      const table = view.table
      // 4a. A finished table is a record, and the record is what Audit is.
      if (table.finished) {
        go({ view: 'table', sid: table.sid, tab: 'audit' }, { replace: true })
        return
      }
      // 4b. The felt with nothing on it. `over` is excluded deliberately:
      //     TableOver covers the felt with the end of the table, and that is
      //     the moment it exists for.
      if (!table.dealing && !table.over) {
        go({ view: 'table', sid: table.sid, tab: 'now' }, { replace: true })
      }
    }
  }, [route, view, state])
}
