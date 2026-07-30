import { useEffect, useMemo, useReducer, useState } from 'react'
import { api, type Bond as BondInfo, type HandView, type Snapshot } from './api'
import { useHost } from './host'
import { useTableState } from './state'
import { Bond } from './table/Bond'
import { Lifecycle } from './table/Lifecycle'
import { Money } from './table/Money'
import { Progress } from './table/Progress'
import { OnChain } from './table/OnChain'
import { Provenance } from './table/Provenance'
import { Verify } from './table/Verify'
import { ActionBar } from './felt/ActionBar'
import { Status } from './felt/Status'
import { Table } from './felt/Table'
import { TableOver } from './felt/TableOver'

// One table, two ways of standing at it.
//
// While a table is being formed and funded, the scrolling view leads: the step
// rail says what to do next and the cards under it are the full account. Once
// it deals, the stage takes over - the table filling the window, the verify
// rail at its elbow, one status line and one action bar that never move. The
// detail view stays a click away as Details, because the money cards and the
// Leave button live there and hiding them would trade the wrong thing.

type Tab = 'play' | 'details'

/** holdFor is how long a finished hand stays on screen after it ends.
 *
 *  A showdown was over in about a second: the cards opened, the next hand's
 *  preparations were already running, and the screen moved on before anybody
 *  could read what they had just been shown. Nothing is paused to fix that -
 *  the table goes on shuffling behind the held picture, since a hand that has
 *  ended cannot be affected by being looked at. A hold on what is rendered,
 *  not a delay to anything played. */
const holdFor = 15_000

/** useShowdownHold keeps the hand that just finished on screen for a while. */
function useShowdownHold(hand?: HandView): { view: HandView; left: number } | undefined {
  const [held, setHeld] = useState<{ view: HandView; until: number }>()
  const [, tick] = useReducer((n: number) => n + 1, 0)

  useEffect(() => {
    if (hand?.done) setHeld({ view: hand, until: Date.now() + holdFor })
  }, [hand])

  useEffect(() => {
    if (!held) return
    const id = window.setInterval(tick, 250)
    return () => window.clearInterval(id)
  }, [held])

  if (!held) return undefined
  const left = held.until - Date.now()
  if (left <= 0) return undefined
  return { view: held.view, left: Math.ceil(left / 1000) }
}

function readTab(): Tab {
  return window.location.hash.replace('#', '') === 'details' ? 'details' : 'play'
}

export function App() {
  const host = useHost()
  const state = useTableState(host.ready)
  const [tab, setTab] = useState<Tab>(readTab)
  const [chosen, setChosen] = useState<string>()
  // Held here because two things need it: the step rail, which asks whether
  // this player can join anything at all, and the card that says when it comes
  // back. Asking twice would be two answers on two clocks.
  const [bond, setBond] = useState<BondInfo>()

  useEffect(() => {
    const onHash = () => setTab(readTab())
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])

  const show = (next: Tab) => {
    setTab(next)
    // replaceState rather than assigning the hash, so switching views does
    // not fill the history of a panel nobody can press Back in.
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
  const liveHand = sid ? state.hands[sid] : undefined
  const holding = useShowdownHold(liveHand)
  const ledger = sid ? state.ledgers[sid] : undefined

  useEffect(() => {
    if (!table) return
    host.title(table.dealing ? `hand ${table.hand ?? 0}` : table.state)
  }, [table, host])

  const roster = ledger?.roster ?? table?.roster ?? []
  const names = useMemo(() => {
    const m = new Map<number, string>()
    for (const s of roster) if (s.name) m.set(s.seat, s.name)
    return m
  }, [roster])

  // The stage leads whenever there is a game to stand at; the held showdown
  // counts, because that is the moment worth looking at.
  const hand = holding?.view ?? liveHand
  const stageOn = Boolean(table && !table.finished && (table.dealing || holding))

  if (stageOn && table && tab !== 'details') {
    const winners =
      holding && (holding.view.awards?.length ?? 0) > 0
        ? (holding.view.awards ?? [])
            .filter((a) => {
              const paid = holding.view.chairs?.find((c) => c.seat === a.seat)?.total ?? 0
              return a.atoms > paid
            })
            .map((a) => a.seat)
        : undefined

    return (
      <div className="stage">
        <Verify table={table} ledger={ledger} hand={hand} names={names} />
        <div className="arena">
          <header className="topbar">
            <span className="hand-no">hand {hand?.hand ?? table.hand ?? '—'}</span>
            {hand?.street && hand.phase === 'betting' && <span>{hand.street}</span>}
            <span className="spacer" />
            <span className="live">
              <span className={`dot ${state.live ? 'on' : 'off'}`} />
              {state.live ? 'live' : 'polling'}
            </span>
            <button className="link" onClick={() => show('details')}>
              Details
            </button>
          </header>
          <div className="table-zone">
            <Table
              hand={hand}
              roster={roster}
              ourSeat={table.seat}
              stacks={table.live ?? table.settled?.stacks}
              won={winners}
            />
            {/* The end of the table, after the last hand has had its fifteen
              * seconds. It covers the felt rather than the whole stage, so the
              * verify rail keeps saying what the payout below is made of. */}
            {table.over && !holding && (
              <TableOver
                table={table}
                ledger={ledger}
                onDetails={() => show('details')}
                onClose={host.close}
              />
            )}
          </div>
          <Status table={table} hand={hand} holdingLeft={holding?.left} names={names} />
          <ActionBar hand={holding ? undefined : liveHand} />
        </div>
      </div>
    )
  }

  return (
    <div className="app">
      <nav className="bar">
        {stageOn && (
          <button className="tab" onClick={() => show('play')}>
            ← Back to the table
          </button>
        )}
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
                {t.sid.slice(0, 8)} · {t.finished ? 'finished' : t.state}
              </button>
            ))}
          </div>
        )}

        {!table ? (
          <>
            <Nothing ready={host.ready} error={state.error} />
            {host.ready && <Bond />}
          </>
        ) : (
          <>
            {table.finished && <Finished table={table} />}
            {!table.finished && (
              <Progress table={table} ledger={ledger} bond={bond} onFelt={() => show('play')} />
            )}
            <Provenance hand={hand} />
            <Money table={table} ledger={ledger} />
            <OnChain ledger={ledger} dealing={table.dealing} accused={ledger?.accused} over={table.over} />
            <Lifecycle table={table} />
            <Bond onLoad={setBond} />
          </>
        )}
      </main>
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

/** Finished is a table this player has got up from, kept only because it still
 *  holds coin. The log is offered here because this is the moment somebody
 *  wants it: it is the account of what happened that needs nobody to be
 *  believed, and it is what a dispute would be argued from. */
function Finished({ table }: { table: Snapshot }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()

  const save = () => {
    setBusy(true)
    setError(undefined)
    api
      .log(table.sid)
      .then((text) => {
        const url = URL.createObjectURL(new Blob([text], { type: 'application/json' }))
        const link = document.createElement('a')
        link.href = url
        link.download = `poker-${table.sid}.json`
        link.click()
        URL.revokeObjectURL(url)
      })
      .catch((e) => setError(String(e instanceof Error ? e.message : e)))
      .finally(() => setBusy(false))
  }

  return (
    <section className="card">
      <h2>Finished</h2>
      <p className="headline">You have left this table.</p>
      <p className="lede">
        It is kept because it still holds coin of yours on the chain. Nothing is dealt here
        and nobody can be seated at it again. What is below is the record.
      </p>
      <div className="row">
        <span>The signed log this table was played from</span>
        <button className="act" disabled={busy} onClick={save}>
          {busy ? 'fetching…' : 'Save the log'}
        </button>
      </div>
      <p className="lede muted">
        Every entry carries the signature of the seat that made it and the chain hashes
        forward, so somebody who was not here and trusts nobody who was can check it.
      </p>
      {error && <p className="lede bad">{error}</p>}
    </section>
  )
}
