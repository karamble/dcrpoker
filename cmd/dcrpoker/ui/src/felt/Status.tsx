import { useEffect, useReducer } from 'react'
import type { HandView, Snapshot } from '../api'
import { dcr } from '../format'

// One line that always says where things stand, in one fixed place.
//
// It used to be scattered: the result appeared in one card, "the seats are
// signing" in another, "waiting for your cards" above the buttons - each
// appearing and disappearing, each changing the height of everything below it,
// so the buttons jumped and the player scrolled to find out what had happened.
// A fixed line whose words change is the whole fix.
//
// The result guards against absent awards, and the guard is load-bearing:
// absent means this peer does not know yet - a showdown still owed a card -
// and reading absence as zero once told both players they had lost the same
// hand, which no hand can do.

/** useCountdown is the seconds left, counted here and nowhere else.
 *
 *  The hold used to run a 250ms interval at the top of the tree, re-rendering
 *  every card and every seat four times a second to move one number. The
 *  deadline is a prop now and the ticking is this leaf's. */
function useCountdown(until?: number): number | undefined {
  const [, tick] = useReducer((n: number) => n + 1, 0)

  useEffect(() => {
    if (until === undefined) return
    const id = window.setInterval(tick, 250)
    return () => window.clearInterval(id)
  }, [until])

  if (until === undefined) return undefined
  return Math.max(0, Math.ceil((until - Date.now()) / 1000))
}

export function Status({
  table,
  hand,
  holdUntil,
  names,
}: {
  table: Snapshot
  hand?: HandView
  /** When the showdown hold ends, while a finished hand is being shown. */
  holdUntil?: number
  names: Map<number, string>
}) {
  const holdingLeft = useCountdown(holdUntil)
  const who = (seat: number) => names.get(seat) || `seat ${seat}`

  // A table that is over serves a between-hands view with done=true and no
  // awards forever, which is not a showdown short a card - it is the end. The
  // overlay above carries the full account; this line only has to not lie.
  // During the showdown hold the result line below still runs, because the
  // last hand's outcome is exactly what those fifteen seconds are for.
  if (table.over && holdingLeft === undefined) {
    return (
      <div className="status">
        <span>The table has ended — the payout is on its way to the chain.</span>
      </div>
    )
  }

  if (hand?.done) {
    const awards = hand.awards ?? []
    if (awards.length === 0) {
      return (
        <div className="status">
          <span>
            The hand is over — the result settles when the last card everybody is owed
            arrives.
          </span>
        </div>
      )
    }
    const paid = hand.chairs?.find((c) => c.seat === hand.seat)?.total ?? 0
    const net = (awards.find((a) => a.seat === hand.seat)?.atoms ?? 0) - paid
    return (
      <div className="status">
      <span className={net > 0 ? 'won' : net < 0 ? 'lost' : undefined}>
        {net > 0 ? `You won ${dcr(net)} DCR` : net < 0 ? `You lost ${dcr(-net)} DCR` : 'You broke even'}
      </span>
        {holdingLeft !== undefined && (
          <span className="count">next hand is being prepared · {holdingLeft}s</span>
        )}
      </div>
    )
  }

  const line = () => {
    if (!table.dealing) return 'The table deals when every stake and bond is on the chain.'
    if (!hand) return 'The seats are signing the last result — nothing is final until they all have.'
    if (hand.phase === 'shuffling')
      return 'Shuffling — every seat permutes the deck in turn and proves it changed nothing else.'
    if (hand.phase === 'dealing')
      return 'Dealing — each seat publishes what the others need to read their own cards.'
    if (hand.phase === 'betting') {
      if ((hand.hole ?? []).length < 2) return 'Waiting for your cards.'
      if (hand.ours)
        return hand.toCall > 0 ? `Your turn — ${dcr(hand.toCall)} to call.` : 'Your turn.'
      if (hand.toAct >= 0) return `Waiting for ${who(hand.toAct)}.`
    }
    if (hand.phase === 'showdown') return 'Showdown — the hands that stayed in are opening.'
    return 'The table is playing.'
  }

  return (
    <div className="status">
      <span>{line()}</span>
    </div>
  )
}
