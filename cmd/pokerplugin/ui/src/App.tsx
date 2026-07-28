import { useEffect, useMemo, useState } from 'react'
import { dcr } from './format'
import { useHost } from './host'
import { useTableState } from './state'
import { Lifecycle } from './table/Lifecycle'
import { Money } from './table/Money'
import { Obligations } from './table/Obligations'
import { OnChain } from './table/OnChain'
import { Provenance } from './table/Provenance'
import { Actions } from './felt/Actions'
import { Felt } from './felt/Felt'

// Two views of one table.
//
// The table view is first and is the default, because it answers the questions
// that decide whether somebody should trust this at all: where the deck came
// from, where the money is, what happened on the chain. The felt is second.
// That ordering is the argument the whole interface is making.

type Tab = 'table' | 'felt'

/** readTab picks the opening tab from the fragment, so a particular view can be
 *  linked to and reloaded back into. The dashboard's own sections do the same
 *  thing with the same mechanism. */
function readTab(): Tab {
  return window.location.hash.replace('#', '') === 'felt' ? 'felt' : 'table'
}

export function App() {
  const host = useHost()
  const state = useTableState(host.ready)
  const [tab, setTab] = useState<Tab>(readTab)
  const [chosen, setChosen] = useState<string>()

  useEffect(() => {
    const onHash = () => setTab(readTab())
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])

  const show = (next: Tab) => {
    setTab(next)
    // replaceState rather than assigning the hash, so switching tabs does not
    // fill the history of a panel somebody cannot press Back in anyway.
    window.history.replaceState(null, '', `#${next}`)
  }

  // Which table. The host names one when the panel is opened from an
  // invitation; otherwise the first, which is almost always the only one.
  const sid = useMemo(() => {
    const ids = state.tables.map((t) => t.sid)
    if (chosen && ids.includes(chosen)) return chosen
    if (host.tableId && ids.includes(host.tableId)) return host.tableId
    return ids[0]
  }, [state.tables, chosen, host.tableId])

  const table = state.tables.find((t) => t.sid === sid)
  const hand = sid ? state.hands[sid] : undefined
  const ledger = sid ? state.ledgers[sid] : undefined

  useEffect(() => {
    if (!table) return
    host.title(table.dealing ? `hand ${table.hand ?? 0}` : table.state)
  }, [table, host])

  return (
    <div className="app">
      <nav className="bar">
        <button className="tab" role="tab" aria-selected={tab === 'table'} onClick={() => show('table')}>
          The table
        </button>
        <button className="tab" role="tab" aria-selected={tab === 'felt'} onClick={() => show('felt')}>
          The felt
        </button>
        <span className="bar-spacer" />
        <span className="live">
          <span className={`dot ${state.live ? 'on' : 'off'}`} />
          {state.live ? 'live' : 'asking every 2s'}
        </span>
      </nav>

      <main className="body">
        {state.error && <div className="banner">{state.error}</div>}

        {state.tables.length > 1 && (
          <div className="picker">
            {state.tables.map((t) => (
              <button
                key={t.sid}
                className="pick"
                aria-pressed={t.sid === sid}
                onClick={() => setChosen(t.sid)}
              >
                {t.sid.slice(0, 8)} · {t.state}
              </button>
            ))}
          </div>
        )}

        {!table ? (
          <Nothing ready={host.ready} error={state.error} />
        ) : tab === 'table' ? (
          <>
            <Provenance hand={hand} />
            <Money table={table} ledger={ledger} />
            <OnChain ledger={ledger} />
            <Obligations table={table} ledger={ledger} payoutAddress={host.payoutAddress} />
            <Lifecycle table={table} />
          </>
        ) : !table.dealing || !hand ? (
          // The plugin answers 400 on /table/hand until the table deals, which
          // is a state and not a failure - so this asks the snapshot instead
          // and never calls that route at all.
          <section className="card">
            <h2>The felt</h2>
            <p className="lede">
              Nothing is being dealt yet. {table.funded} of {table.seats} stakes and{' '}
              {table.bonded} of {table.seats} bonds are on the chain, and the table deals
              when both are complete.
            </p>
            <p className="lede muted">
              The buy-in is {dcr(table.buyinAtoms)} DCR a seat. Everything outstanding is
              on the table view.
            </p>
          </section>
        ) : (
          <section className="card">
            <div className="felt-wrap">
              <Felt hand={hand} roster={ledger?.roster ?? table.roster ?? []} />
              {hand.done ? (
                <Done hand={hand} />
              ) : (
                <Actions hand={hand} />
              )}
            </div>
          </section>
        )}
      </main>
    </div>
  )
}

function Done({ hand }: { hand: ReturnType<typeof useTableState>['hands'][string] }) {
  return (
    <div className="rows">
      <p className="waiting">The hand is over.</p>
      {(hand.awards ?? []).map((a) => (
        <div className="row" key={a.seat}>
          <span>
            seat {a.seat}
            {a.seat === hand.seat ? ' · you' : ''}
          </span>
          <span>{dcr(a.atoms)} DCR</span>
        </div>
      ))}
    </div>
  )
}

function Nothing({ ready, error }: { ready: boolean; error?: string }) {
  if (!ready) {
    return (
      <section className="card">
        <h2>Waiting for the host</h2>
        <p className="lede">
          This page is served by the poker plugin and driven by the dashboard around it.
          It is waiting to be handed a table and a token.
        </p>
      </section>
    )
  }
  return (
    <section className="card">
      <h2>No table</h2>
      <p className="lede">
        This player is not at a table. Accepting an invitation in a group chat is what
        starts one.
      </p>
      {error && <p className="lede bad">{error}</p>}
    </section>
  )
}
