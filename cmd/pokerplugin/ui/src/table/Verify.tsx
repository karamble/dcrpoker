import { useState } from 'react'
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

const RAIL_KEY = 'poker.vrail'

export function Verify({
  table,
  ledger,
  hand,
  names,
}: {
  table: Snapshot
  ledger?: LedgerView
  hand?: HandView
  names: Map<number, string>
}) {
  const [open, setOpen] = useState<boolean>(() => {
    try {
      return window.localStorage.getItem(RAIL_KEY) !== 'closed'
    } catch {
      return true
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

  const roster = ledger?.roster ?? table.roster ?? []
  const who = (seat: number) => names.get(seat) || `seat ${seat}`
  const settled = ledger?.settled ?? table.settled
  const live = ledger?.live ?? table.live
  const shuffles = hand?.shuffles ?? []
  const deckDone = shuffles.length > 0 && shuffles.every((s) => s.state !== 'awaited')

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
