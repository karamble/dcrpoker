import { useEffect, useMemo, useReducer, useState } from 'react'
import { api, type Bond as BondInfo, type HandView, type Snapshot } from './api'
import { dcr } from './format'
import { useHost } from './host'
import { useTableState } from './state'
import { Bond } from './table/Bond'
import { Lifecycle } from './table/Lifecycle'
import { Money } from './table/Money'
import { Progress } from './table/Progress'
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
/** holdFor is how long a finished hand stays on screen after it ends.
 *
 *  A showdown was over in about a second: the cards opened, the next hand's
 *  preparations were already running, and the screen moved on before anybody
 *  could read what they had just been shown. The one moment the interface has
 *  to display its working was the one moment it did not.
 *
 *  Nothing is paused to do this. The table goes on shuffling and dealing behind
 *  it - a hand that has ended cannot be affected by looking at it - so this is
 *  a fifteen second hold on what is *rendered*, and no delay at all to what is
 *  played. */
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
  return window.location.hash.replace('#', '') === 'felt' ? 'felt' : 'table'
}

export function App() {
  const host = useHost()
  const state = useTableState(host.ready)
  const [tab, setTab] = useState<Tab>(readTab)
  const [chosen, setChosen] = useState<string>()
  // Held here because two things need it: the step rail, which asks whether
  // this player can join anything at all, and the card at the bottom that says
  // when it comes back. Asking twice would be two answers on two clocks.
  const [bond, setBond] = useState<BondInfo>()

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
  const holding = useShowdownHold(hand)
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
        ) : tab === 'table' ? (
          <>
            {table.finished && <Finished table={table} />}
            {!table.finished && (
              <Progress table={table} ledger={ledger} bond={bond} onFelt={() => show('felt')} />
            )}
            <Provenance hand={hand} />
            <Money table={table} ledger={ledger} />
            <OnChain ledger={ledger} dealing={table.dealing} refreshed={ledger?.refreshed} />
            <Lifecycle table={table} />
            <Bond onLoad={setBond} />
          </>
        ) : holding ? (
          // The hand that just ended, kept up long enough to read. The table is
          // not waiting for this - it is already shuffling the next one.
          <section className="card">
            <div className="felt-wrap">
              <Felt hand={holding.view} roster={ledger?.roster ?? table.roster ?? []} />
              <Done hand={holding.view} left={holding.left} />
            </div>
          </section>
        ) : !table.dealing || !hand ? (
          // The plugin answers 400 on /table/hand until the table deals, which
          // is a state and not a failure - so this asks the snapshot instead
          // and never calls that route at all.
          <section className="card">
            <h2>The felt</h2>
            <p className="lede">
              {table.dealing
                ? 'The last hand is over and the seats are signing the result. The next hand ' +
                  'starts when they agree - nothing here is final until they do.'
                : `Nothing is being dealt yet. ${table.funded} of ${table.seats} stakes and ` +
                  `${table.bonded} of ${table.seats} bonds are on the chain, and the table ` +
                  'deals when both are complete.'}
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

/** Done says what the hand did to this player, in those words.
 *
 *  It used to say "the hand is over" and then list what each seat was awarded,
 *  which is the reducer's vocabulary rather than a person's: an award is not a
 *  result. A seat that put in 1000 and is awarded 1000 won nothing, and the old
 *  screen showed it the same way it showed a seat that was awarded 1000 having
 *  put in nothing. What somebody wants is the difference. */
function Done({
  hand,
  left,
}: {
  hand: ReturnType<typeof useTableState>['hands'][string]
  left?: number
}) {
  const awards = hand.awards ?? []
  const paid = hand.chairs?.find((c) => c.seat === hand.seat)?.total ?? 0
  const net = (awards.find((a) => a.seat === hand.seat)?.atoms ?? 0) - paid

  // No awards means the result is not known here yet, which is not the same as
  // having won nothing - and reading it as nothing told both seats they had
  // lost, which cannot be true of the same hand.
  //
  // They are absent while a showdown is still short a card: Settle refuses to
  // guess and the plugin sends no awards rather than wrong ones. So this waits
  // for them instead of doing the arithmetic on a missing number.
  if (awards.length === 0) {
    return (
      <div className="rows">
        <p className="headline">The hand is over</p>
        <p className="lede">
          What it came to is still being worked out - a showdown settles when the last
          card everybody is owed has arrived, and nothing here guesses at it before then.
        </p>
      </div>
    )
  }

  return (
    <div className="rows">
      <p className="headline">
        {net > 0 ? `You won ${dcr(net)} DCR` : net < 0 ? `You lost ${dcr(-net)} DCR` : 'You broke even'}
      </p>
      {awards.map((a) => {
        const seatPaid = hand.chairs?.find((c) => c.seat === a.seat)?.total ?? 0
        const seatNet = a.atoms - seatPaid
        return (
          <div className="row" key={a.seat}>
            <span>
              seat {a.seat}
              {a.seat === hand.seat ? ' · you' : ''}
            </span>
            <span className={seatNet > 0 ? 'good' : seatNet < 0 ? 'muted' : undefined}>
              {seatNet > 0 ? `+${dcr(seatNet)}` : seatNet < 0 ? `-${dcr(-seatNet)}` : '±0'} DCR
            </span>
          </div>
        )
      })}
      <p className="lede muted">
        The seats are signing this result now. Nothing is final until they all have, and
        the next hand starts when they agree.
        {left !== undefined
          ? ` The next hand is already being shuffled; this stays up for ${left}s.`
          : ''}
      </p>
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
