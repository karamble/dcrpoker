import { useEffect, useState } from 'react'
import { api, type HandView } from '../api'
import { atoms, dcr } from '../format'

// Taking a turn.
//
// Every button here comes from the `legal` list the plugin sent, in the order
// it sent them, and nothing is inferred. That list is derived from the same
// state the rules are checked against, so an action offered here is one the
// rules will accept - and an action not offered is one that would be refused,
// which is a better thing to know before pressing than after.
//
// Fold is last, always, because the plugin puts it last, because it is the one
// move that is never the answer to a question the player was not asked.
//
// Amounts are the seat's total commitment for the street after the move, not
// the difference. That is what the protocol takes, and converting here would
// mean two places that have to agree about what a raise means.

const labels: Record<string, string> = {
  check: 'Check',
  call: 'Call',
  bet: 'Bet',
  raise: 'Raise',
  allin: 'All in',
  fold: 'Fold',
}

export function Actions({ hand }: { hand: HandView }) {
  // Held as the text somebody typed, in DCR, because that is the unit
  // everything else on this screen is in. It becomes atoms once, on the way
  // out - see format.atoms.
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  const [refused, setRefused] = useState<string>()

  const mine = hand.chairs?.find((c) => c.seat === hand.seat)
  const floor = hand.toCall > 0 ? hand.toCall + hand.minRaise : hand.minRaise
  const ceiling = mine ? mine.stack + mine.committed : undefined

  useEffect(() => {
    // Start at the smallest legal raise for this spot, which is the number
    // most people want and the one that is most annoying to work out.
    setTyped(dcr(floor))
    setRefused(undefined)
  }, [hand.hand, hand.street, hand.toAct, floor])

  if (!hand.ours) {
    return (
      <p className="waiting">
        {hand.toAct >= 0 ? `Seat ${hand.toAct} is to act.` : 'Nothing to act on.'}
      </p>
    )
  }

  const legal = hand.legal ?? []
  const needsAmount = legal.includes('bet') || legal.includes('raise')

  const send = (action: string) => {
    setBusy(true)
    setRefused(undefined)
    const value = action === 'bet' || action === 'raise' ? atoms(typed) : 0
    api
      .act(hand.sid, action, value)
      .catch((e) => setRefused(String(e instanceof Error ? e.message : e)))
      .finally(() => setBusy(false))
  }

  return (
    <div className="rows">
      <div className="actions">
        {needsAmount && (
          <label className="amount-field">
            <input
              className="amount"
              type="text"
              inputMode="decimal"
              value={typed}
              aria-label="total for this street, in DCR"
              onChange={(e) => setTyped(e.target.value)}
            />
            <span className="unit">DCR</span>
          </label>
        )}
        {legal.map((action) => (
          <button
            key={action}
            className={`act${action === 'fold' ? ' fold' : ''}${
              action === 'call' || action === 'check' ? ' primary' : ''
            }`}
            disabled={busy}
            onClick={() => send(action)}
          >
            {labels[action] ?? action}
            {action === 'call' && hand.toCall > 0 ? ` ${dcr(hand.toCall)}` : ''}
          </button>
        ))}
      </div>
      {needsAmount && (
        <p className="lede muted">
          The amount is this seat&rsquo;s total for the street after the move, not the
          difference. The smallest legal one here is {dcr(floor)} DCR
          {ceiling !== undefined ? `, and everything you have is ${dcr(ceiling)}` : ''}.
        </p>
      )}
      {refused && <p className="lede bad">{refused}</p>}
    </div>
  )
}
