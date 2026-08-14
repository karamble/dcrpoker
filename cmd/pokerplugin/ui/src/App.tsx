import { useEffect } from 'react'
import { MotionConfig } from 'framer-motion'
import { useBond } from './bond'
import { useRouteGuards } from './guards'
import { go, useRoute } from './route'
import { useTableState } from './state'
import { useTableView } from './view'
import { Chrome } from './shell/Chrome'
import { Felt } from './felt/Felt'
import { Lobby } from './lobby/Lobby'
import { TableView } from './table/TableView'

// Three views and nothing else.
//
// The lobby is what a player comes back to; a table view is where the money and
// the record are; the felt is where the cards are. Which one is showing is the
// address bar's answer and not this component's, so a reload, a bookmark and
// Back all land where they should - see route.ts.
//
// State is gathered once here and handed down. The guards, the felt and the
// tabs all need the same answer to "what is this table doing", and three of
// them working it out separately is three chances to disagree during a hand.

export function App() {
  const state = useTableState()
  const route = useRoute()
  const view = useTableView(state, route.view === 'lobby' ? undefined : route.sid)
  const bond = useBond()
  useRouteGuards(route, view, state)

  const table = view.table
  useEffect(() => {
    // Standalone, the tab strip is the only place the table can announce
    // itself while somebody is looking at something else.
    const what = !table
      ? undefined
      : table.finished
        ? 'finished'
        : table.dealing
          ? `hand ${table.hand ?? 0}`
          : table.state
    document.title = what ? `${what} · poker` : 'poker'
  }, [table])

  return (
    <MotionConfig reducedMotion="user">
      {route.view === 'lobby' ? (
        <Chrome live={state.live} error={state.error}>
          <Lobby state={state} bond={bond} />
        </Chrome>
      ) : !table ? (
        <Chrome live={state.live} error={state.error} lead={<ToLobby />}>
          <Missing loaded={state.loaded} />
        </Chrome>
      ) : route.view === 'felt' ? (
        <Felt table={table} view={view} live={state.live} />
      ) : (
        <Chrome
          live={state.live}
          error={state.error}
          roster={view.roster}
          seat={view.seat}
          lead={<ToLobby />}
        >
          <TableView route={route} view={view} bond={bond} />
        </Chrome>
      )}
    </MotionConfig>
  )
}

/** ToLobby is the way back, and the only way to reach the standing bond and the
 *  seed while seated. */
function ToLobby() {
  return (
    <button className="tab" onClick={() => go({ view: 'lobby' })}>
      ← Tables
    </button>
  )
}

/** Missing covers the moment between an address naming a table and this page
 *  knowing whether there is one. A guard sends it back to the lobby once the
 *  first answer has come in; until then, saying nothing would look like a
 *  broken page. */
function Missing({ loaded }: { loaded: boolean }) {
  return (
    <section className="card">
      <h2>{loaded ? 'No such table' : 'Looking'}</h2>
      <p className="lede">
        {loaded
          ? 'The address names a table this player is not at. Going back to the tables.'
          : 'Asking the plugin which tables this player is at.'}
      </p>
    </section>
  )
}
