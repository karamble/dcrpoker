import { useState } from 'react'
import { api } from '../api'
import type { HandView, LedgerView, Snapshot } from '../api'
import { dcr, short } from '../format'

// What only this client can show, beside the game instead of behind it.
//
// Every poker site says "trust us". This table can show its working - where the
// deck came from, who verified what, where the money actually sits and what
// binds it - and before this rail that account lived in cards two tabs away
// that nobody read while playing. The rail keeps it at the table's elbow.
//
// The discipline carried over from those cards, because it is the whole point:
// never overstate. "Ours" is a stronger claim than "verified"; Signed is a fact
// and Now is a promise; an outpoint shown here is one this peer checked against
// the chain itself, not one it was told about.
//
// Two shapes. The rail is the felt's, where all of it is at a glance and none of
// it is in the way. The card is the audit tab's, and it carries the part that
// cannot be got at any other way: whether the log is in step, and the challenge.
// The deck, the money and the escrow are three whole cards on that tab already,
// and a second telling of them there would be a page arguing with itself. Once a
// table ends the felt is unreachable, so without the card the challenge would be
// too.

const RAIL_KEY = 'poker.vrail'

export function Verify({
  table,
  ledger,
  hand,
  names,
  variant = 'rail',
}: {
  table: Snapshot
  ledger?: LedgerView
  hand?: HandView
  names: Map<number, string>
  variant?: 'rail' | 'card'
}) {
  // Collapsed by default: the rail is where a curious player checks the
  // deck and the escrow, not something they need in their way to play. It
  // opens on a click and then remembers the choice, so somebody who wants it
  // open keeps it.
  const [open, setOpen] = useState<boolean>(() => {
    try {
      return window.localStorage.getItem(RAIL_KEY) === 'open'
    } catch {
      return false
    }
  })
  const toggle = () => {
    setOpen((was) => {
      try {
        window.localStorage.setItem(RAIL_KEY, was ? 'closed' : 'open')
      } catch {
        // Remembering is a nicety.
      }
      return !was
    })
  }

  const [challengePending, setChallengePending] = useState(false)
  const [challengeError, setChallengeError] = useState<string | undefined>(undefined)
  const roster = ledger?.roster ?? table.roster ?? []
  const who = (seat: number) => names.get(seat) || `seat ${seat}`
  const settled = ledger?.settled ?? table.settled
  const live = ledger?.live ?? table.live
  const shuffles = hand?.shuffles ?? []
  const checked = shuffles.filter((s) => s.state !== 'awaited').length
  const deckDone = shuffles.length > 0 && checked === shuffles.length
  const haveOpenChallenge = (ledger?.challenges ?? []).some((c) => c.open)

  const challenge = () => {
    if (!settled) return
    setChallengePending(true)
    setChallengeError(undefined)
    api
      .challenge(table.sid, settled.hand)
      .catch((e) => setChallengeError(e instanceof Error ? e.message : String(e)))
      .finally(() => setChallengePending(false))
  }

  if (variant === 'card') {
    return (
      <section className="card quiet">
        <h2>Checking it yourself</h2>
        <div className="rows">
          <div className="row">
            <span>The log</span>
            <span className={table.waiting > 0 ? 'warn' : 'good'}>
              {table.waiting > 0
                ? `${table.waiting} entries waiting on one before them`
                : 'in step'}
            </span>
          </div>
          {shuffles.length > 0 && (
            <div className="row">
              <span>Shuffles this process checked</span>
              <span>
                {checked} of {shuffles.length}
                {hand?.hand ? ` · hand ${hand.hand}` : ''}
              </span>
            </div>
          )}
        </div>
        <p className="lede muted">
          Every action is signed by its seat and chains forward. This is what a dispute
          would be argued from, and it needs nobody to be believed.
        </p>
        {settled && settled.hand > 0 && (
          <>
            <div className="actions">
              <button
                className="act"
                disabled={challengePending || haveOpenChallenge}
                onClick={challenge}
              >
                {haveOpenChallenge
                  ? 'A hand is already challenged'
                  : `Challenge hand ${settled.hand}`}
              </button>
            </div>
            <p className="lede muted">
              Any seat may demand a settled hand be recomputed from everyone's secrets.
              Refusing costs the bond — and the challenged hand shows its cards to this
              table, folds included, yours too.
            </p>
            {challengeError && (
              <p className="lede bad">The challenge did not reach the table: {challengeError}</p>
            )}
          </>
        )}
      </section>
    )
  }

  if (!open) {
    return (
      <aside className="vrail closed" onClick={toggle} title="what only we can show">
        <button className="vrail-toggle" aria-label="open the verify rail">
          ›
        </button>
        <span className={`vrail-glyph${deckDone ? ' ok' : ''}`} title="the deck">
          ♠
        </span>
        <span className="vrail-glyph ok" title="the escrow">
          ⛓
        </span>
      </aside>
    )
  }

  return (
    <aside className="vrail">
      <section>
        <h3>
          the deck
          <button className="vrail-toggle" onClick={toggle} aria-label="collapse">
            ‹
          </button>
        </h3>
        {shuffles.length === 0 ? (
          <span className="vr-note">A fresh deck is made for every hand.</span>
        ) : (
          shuffles.map((s) => (
            <div className="vr-line" key={s.seat}>
              <span>{who(s.seat)}</span>
              <b className={s.state === 'awaited' ? 'vr-wait' : 'vr-ok'}>
                {s.state === 'ours'
                  ? 'shuffled · ours'
                  : s.state === 'verified'
                    ? 'proof verified'
                    : 'awaited'}
              </b>
            </div>
          ))
        )}
        <span className="vr-note">
          Encrypted and permuted by every seat in turn, with a proof each time.
          Nobody has seen a card nobody may see.
        </span>
      </section>

      <section>
        <h3>the money</h3>
        {roster.map((s) => (
          <div className="vr-line" key={s.seat}>
            <span>{who(s.seat)}</span>
            <b>
              {settled?.stacks?.[s.seat] !== undefined ? dcr(settled.stacks[s.seat]) : '—'}
              {live?.[s.seat] !== undefined && live[s.seat] !== settled?.stacks?.[s.seat]
                ? ` → ${dcr(live[s.seat])}`
                : ''}
            </b>
          </div>
        ))}
        <span className="vr-note">
          Signed → in play. The first is what every seat put their name to
          {settled ? ` at hand ${settled.hand}` : ''}; the second is a promise until they
          sign again.
        </span>
        {settled && settled.hand > 0 && (
          <>
            <button
              className="ghost"
              disabled={challengePending || haveOpenChallenge}
              onClick={challenge}
            >
              {haveOpenChallenge ? 'a hand is challenged' : `challenge hand ${settled.hand}`}
            </button>
            <span className="vr-note">
              Any seat may demand a settled hand be recomputed from everyone's secrets.
              Refusing costs the bond — and the challenged hand shows its cards to this
              table, folds included, yours too.
            </span>
            {challengeError && (
              <span className="vr-note warn">The challenge did not reach the table: {challengeError}</span>
            )}
          </>
        )}
      </section>

      <section>
        <h3>the escrow</h3>
        {roster.map((s) => (
          <div className="vr-line" key={s.seat}>
            <span>{who(s.seat)}</span>
            <b className="mono">
              {s.stake ? short(s.stake, 4) : '—'} · {s.bondAt || s.bond ? short(s.bondAt || s.bond, 4) : '—'}
            </b>
          </div>
        ))}
        <span className="vr-note">
          Stake · bond, as this peer found them on the chain itself. Every escrow needs
          every seat's signature — nobody can move it alone.
        </span>
      </section>

      <section>
        <h3>the log</h3>
        <div className="vr-line">
          <span>hand</span>
          <b>{hand?.hand ?? '—'}</b>
        </div>
        <div className="vr-line">
          <span>entries</span>
          <b className={table.waiting > 0 ? 'vr-wait' : 'vr-ok'}>
            {table.waiting > 0 ? `${table.waiting} out of order` : 'in step'}
          </b>
        </div>
        <span className="vr-note">
          Every action is signed by its seat and chains forward. This is what a dispute
          would be argued from, and it needs nobody to be believed.
        </span>
      </section>
    </aside>
  )
}
