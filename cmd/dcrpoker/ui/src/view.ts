import { useEffect, useMemo, useState } from 'react'
import type { HandView, LedgerView, SeatView, Snapshot } from './api'
import type { State } from './state'

// Everything derived from (state, sid), worked out once.
//
// It is called in App and passed down, so the guards, the felt and the tabs
// cannot disagree about what the table is doing. The alternative - each of them
// picking the table out of state for itself - is three answers to one question,
// and the one that matters here is "is a hand being held on screen", which
// freezes the router.

/** holdFor is how long a finished hand stays on screen after it ends.
 *
 *  A showdown was over in about a second: the cards opened, the next hand's
 *  preparations were already running, and the screen moved on before anybody
 *  could read what they had just been shown. Nothing is paused to fix that -
 *  the table goes on shuffling behind the held picture, since a hand that has
 *  ended cannot be affected by being looked at. A hold on what is rendered,
 *  not a delay to anything played. */
const holdFor = 15_000

export type TableView = {
  table?: Snapshot
  ledger?: LedgerView
  /** The hand to draw: the held showdown while one is being kept on screen,
   *  otherwise the live one. */
  hand?: HandView
  /** The hand as it stands now. The action bar obeys this and not `hand`,
   *  because a held picture is a record and cannot be acted on. */
  liveHand?: HandView
  /** When a finished hand is being held, the moment it stops being. */
  holding?: { view: HandView; until: number }
  roster: SeatView[]
  names: Map<number, string>
  /** This player's seat, when they have one. */
  seat?: number
}

/** useShowdownHold keeps the hand that just finished on screen for a while.
 *
 *  It carries no interval of its own. The number of seconds left is drawn by
 *  Status, which ticks in that one leaf; all this owns is the deadline and the
 *  single re-render when it passes. */
function useShowdownHold(
  sid: string | undefined,
  hand?: HandView,
): { view: HandView; until: number } | undefined {
  const [held, setHeld] = useState<{ view: HandView; until: number }>()

  // A hold belongs to the table it was taken at. Nothing carries across:
  // another table's showdown drawn on this felt would be a picture of a hand
  // that was not played here, and a hold that outlived its table would freeze
  // the guards somewhere they have no business freezing.
  useEffect(() => {
    setHeld(undefined)
  }, [sid])

  useEffect(() => {
    if (!hand?.done) return
    // The poll parses fresh JSON, so the same finished hand arrives as a new
    // object every couple of seconds. Take the newer view - a showdown short a
    // card fills its awards in late, and those awards are what the hold is for
    // - but keep the deadline it was given, or the hold never ends and the
    // guards never move again.
    setHeld((prev) =>
      prev && prev.view.hand === hand.hand
        ? { view: hand, until: prev.until }
        : { view: hand, until: Date.now() + holdFor },
    )
  }, [hand])

  useEffect(() => {
    if (!held) return
    const left = held.until - Date.now()
    if (left <= 0) {
      setHeld(undefined)
      return
    }
    const id = window.setTimeout(() => setHeld(undefined), left)
    return () => window.clearTimeout(id)
  }, [held])

  return held
}

export function useTableView(state: State, sid?: string): TableView {
  const table = sid ? state.tables.find((t) => t.sid === sid) : undefined
  const ledger = sid ? state.ledgers[sid] : undefined
  const liveHand = sid ? state.hands[sid] : undefined
  const holding = useShowdownHold(sid, liveHand)

  const roster = ledger?.roster ?? table?.roster ?? []
  const names = useMemo(() => {
    const m = new Map<number, string>()
    for (const s of roster) if (s.name) m.set(s.seat, s.name)
    return m
  }, [roster])

  return {
    table,
    ledger,
    hand: holding?.view ?? liveHand,
    liveHand,
    holding,
    roster,
    names,
    seat: table?.seat,
  }
}
