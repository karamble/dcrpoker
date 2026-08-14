import { useEffect, useRef, useState } from 'react'
import { AnimatePresence, motion } from 'framer-motion'
import type { Snapshot } from '../api'
import type { BondState } from '../bond'
import type { State } from '../state'
import { Bond } from '../table/Bond'
import { Seed } from '../table/Seed'
import { Empty } from './Empty'
import { Masthead } from './Masthead'
import { Receipts } from './Receipts'
import { TableRow } from './TableRow'
import { WaitingOnYou } from './WaitingOnYou'

// Every table this player is at, and nothing that pretends this game can start
// one.
//
// Live tables lead and finished ones are grouped underneath as receipts. That
// is not a new ordering: finished-last is already the list's first sort key,
// drawn as a heading rather than as a run of dimmer rows. Within each group the
// order is the one state.ts fixed, which this does not touch.
//
// The standing bond and the seed sit at the foot, because they belong to the
// player rather than to any table and this is the only screen that is about the
// player. The seed in particular is the one exit from a lost machine and it has
// to be reachable while seated.

export function Lobby({ state, bond }: { state: State; bond: BondState }) {
  const { rows, hold } = useHeldOrder(state.tables)
  const staggered = useFirstPaint()

  const live = rows.filter((t) => !t.finished)
  const done = rows.filter((t) => t.finished)

  return (
    <>
      <Masthead tables={state.tables} bond={bond.bond} />
      <WaitingOnYou tables={live} hands={state.hands} />

      {state.tables.length === 0 ? (
        <Empty loaded={state.loaded} bond={bond.bond} />
      ) : (
        <>
          {live.length > 0 && (
            <section className="tables">
              <div className="lhead">
                <span>
                  tables <span className="count">{live.length}</span>
                </span>
              </div>
              <motion.ul
                className="lrows"
                onPointerEnter={() => hold(true)}
                onPointerLeave={() => hold(false)}
                onPointerCancel={() => hold(false)}
              >
                <AnimatePresence initial={false} mode="popLayout">
                  {live.map((t, i) => (
                    <TableRow
                      key={t.sid}
                      table={t}
                      hand={state.hands[t.sid]}
                      index={i}
                      staggered={staggered}
                    />
                  ))}
                </AnimatePresence>
              </motion.ul>
            </section>
          )}
          <Receipts tables={done} />
        </>
      )}

      <Bond {...bond} />
      <Seed />
    </>
  )
}

/** useFirstPaint is true for the first render and false ever after, which is
 *  the difference between a list arriving and a list changing. */
function useFirstPaint(): boolean {
  const first = useRef(true)
  useEffect(() => {
    first.current = false
  }, [])
  return first.current
}

/** useHeldOrder keeps the order still while somebody is pointing at it.
 *
 *  state.ts sorts these twice over rather than trust the plugin, because a list
 *  that resorts under a cursor is a list that gets the wrong table pressed and
 *  these rows have money behind them. This is the other half of that argument:
 *  the sort is right, and it must not happen at the moment somebody has decided
 *  which row they are about to press.
 *
 *  Only the order is held. Every number in every row goes on updating, and a
 *  table that arrives while the pointer is inside lands at the end of its group
 *  until the pointer leaves. */
function useHeldOrder(tables: Snapshot[]): { rows: Snapshot[]; hold: (on: boolean) => void } {
  const [held, setHeld] = useState(false)
  const order = useRef<string[]>([])

  // Written during render on purpose: it is the same value for the same input,
  // and an effect would publish the order one frame after the rows drawn from
  // it.
  if (!held) order.current = tables.map((t) => t.sid)

  const rank = new Map(order.current.map((sid, i) => [sid, i] as const))
  // Array.sort is stable, so anything the held order has never seen keeps the
  // position state.ts gave it, at the end.
  const rows = [...tables].sort(
    (a, b) => (rank.get(a.sid) ?? Number.MAX_SAFE_INTEGER) - (rank.get(b.sid) ?? Number.MAX_SAFE_INTEGER),
  )

  return { rows, hold: setHeld }
}
