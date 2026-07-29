import { useEffect, useState } from 'react'
import { api, type HandView } from '../api'
import { atoms, dcr } from '../format'

// Taking a turn, in a bar that never moves.
//
// Every button comes from the `legal` list the plugin sent, in the order it
// sent them, and nothing is inferred: that list is derived from the same state
// the rules are checked against, so an action offered here is one that will be
// accepted. The bar keeps its height whether or not anything may be pressed,
// because a row of buttons that appears and disappears moves everything around
// it - and the one thing a player must always find in the same place is the
// thing they act with.
//
// The guards, each learned at a live table: nothing is offered while the cards
// are still coming; nothing before this seat can read its own hand, since the
// phase can reach betting a moment before our cards open; and an all-in names
// the seat's whole stack, because the reducer refuses one that leaves anything
// behind - it used to send zero, which is not a smaller bet but an impossible
// one.

const labels: Record<string, string> = {
  check: 'Check',
  call: 'Call',
  bet: 'Bet',
  raise: 'Raise',
  allin: 'All in',
  fold: 'Fold',
}

export function ActionBar({ hand }: { hand?: HandView }) {
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  const [refused, setRefused] = useState<string>()

  const mine = hand?.chairs?.find((c) => c.seat === hand.seat)
  const floor = hand ? (hand.toCall > 0 ? hand.toCall + hand.minRaise : hand.minRaise) : 0
  const ceiling = mine ? mine.stack + mine.committed : undefined

  useEffect(() => {
    setTyped(dcr(floor))
    setRefused(undefined)
  }, [hand?.hand, hand?.street, hand?.toAct, floor])

  const ready =
    hand &&
    !hand.done &&
    hand.phase === 'betting' &&
    hand.ours &&
    (hand.hole ?? []).length >= 2

  const legal = ready ? (hand.legal ?? []) : []
  const needsAmount = legal.includes('bet') || legal.includes('raise')

  const send = (action: string) => {
    if (!hand) return
    setBusy(true)
    setRefused(undefined)
    const value =
      action === 'bet' || action === 'raise'
        ? atoms(typed)
        : action === 'allin'
          ? (ceiling ?? 0)
          : 0
    api
      .act(hand.sid, action, value)
      .catch((e) => setRefused(String(e instanceof Error ? e.message : e)))
      .finally(() => setBusy(false))
  }

  return (
    <div className="actbar">
      {legal.length === 0 ? (
        // The bar holds its ground while there is nothing to press; the
        // status line above says why.
        <span className="muted" aria-hidden="true">
          {' '}
        </span>
      ) : (
        <>
          {needsAmount && (
            <>
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
              <button type="button" className="quick" onClick={() => setTyped(dcr(floor))}>
                min
              </button>
              {ceiling !== undefined && (
                <button type="button" className="quick" onClick={() => setTyped(dcr(ceiling))}>
                  max
                </button>
              )}
            </>
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
              {action === 'call' && hand && hand.toCall > 0 ? ` ${dcr(hand.toCall)}` : ''}
              {action === 'allin' && ceiling ? ` ${dcr(ceiling)}` : ''}
            </button>
          ))}
          {refused && <span className="bad">{refused}</span>}
        </>
      )}
    </div>
  )
}
