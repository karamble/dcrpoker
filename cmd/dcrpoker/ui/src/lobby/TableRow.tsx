import { motion, useReducedMotion } from 'framer-motion'
import { ChevronRight } from 'lucide-react'
import type { HandView, Snapshot } from '../api'
import { dcr } from '../format'
import { go } from '../route'
import { standing } from '../standing'
import { press, shift, trow } from './motion'
import { Rolling } from './Rolling'
import { SeatPips } from './SeatPips'
import { who } from './who'

// One table, as one press.
//
// The row never spends. A table with money owed carries the word and lands on
// the Money tab; it does not carry a button that asks the host for a payment.
// The whole reason the list is sorted twice is that the wrong row gets pressed,
// and a wrong press that only navigates is one somebody can walk back from.
//
// The surface is a sibling rather than the row's own background, because it is
// the piece that is meant to travel: it carries the layoutId that a table view
// can pick up to grow the pressed row into its own header. Animating the grid
// box instead would stretch the text inside it.

export function TableRow({
  table,
  hand,
  index,
  staggered,
}: {
  table: Snapshot
  hand?: HandView
  index: number
  /** First paint only. A table arriving at minute forty must not wait a
   *  quarter of a second for being tenth in the list. */
  staggered: boolean
}) {
  const it = standing(table, hand)
  const reduced = useReducedMotion()

  const seat = table.seat
  const holds = seat === undefined ? undefined : table.live?.[seat]
  const signed = seat === undefined ? undefined : table.settled?.stacks?.[seat]
  // Only a table waiting on this player pulses, and it pulses red once a duty
  // has stood for a block - by then it is not a turn, it is a bond at risk.
  const urgent = it.yours && (it.tone === 'hot' || it.tone === 'bad')

  // What this row is worth to this player, which is a different number in each
  // of three situations. A table that has ended paid out at the last signed
  // boundary and is not holding a buy-in any more; saying "at stake" there
  // would be naming money that has already moved.
  const money =
    table.dealing && holds !== undefined
      ? { atoms: holds, label: 'you hold', roll: true }
      : table.over && signed !== undefined
        ? { atoms: signed, label: 'paid out', roll: false }
        : { atoms: table.buyinAtoms, label: seat !== undefined ? 'at stake' : 'buy-in', roll: false }

  const open = () =>
    go(it.goto === 'felt' ? { view: 'felt', sid: table.sid } : { view: 'table', sid: table.sid, tab: it.goto })

  return (
    <motion.li
      className="trow-slot"
      layout
      transition={{ layout: shift }}
      custom={staggered ? index : 0}
      variants={trow}
      initial="initial"
      animate="animate"
      exit="exit"
    >
      <motion.button
        type="button"
        className={`trow ${it.tone}${urgent ? ' turn' : ''}`}
        whileHover={reduced ? undefined : { y: -1 }}
        whileTap={reduced ? undefined : { scale: 0.985 }}
        transition={press}
        onClick={open}
        // The label replaces the children rather than adding to them, so it has
        // to carry everything the row draws - including the pips, whose own
        // label a button label overrides.
        aria-label={
          `${who(table)}. ${it.say}${it.note ? `, ${it.note}` : ''}. ` +
          `${table.joined} of ${table.seats} seats taken, ${table.commits} confirmed. ` +
          `${dcr(money.atoms)} DCR ${money.label}.`
        }
      >
        <motion.span
          className="trow-surface"
          aria-hidden
          layoutId={`surface-${table.sid}`}
          transition={shift}
        />
        <span className="trow-rail" aria-hidden />
        <SeatPips seats={table.seats} joined={table.joined} commits={table.commits} />
        <span className="trow-who">
          <b>{who(table)}</b>
          <small className="mono">
            {table.sid.slice(0, 8)} · {dcr(table.buyinAtoms)} buy-in
          </small>
        </span>
        <span className="state">
          <b>{it.say}</b>
          {it.note && <small>{it.note}</small>}
        </span>
        <span className="figure end">
          <b>{money.roll ? <Rolling atoms={money.atoms} /> : dcr(money.atoms)} DCR</b>
          <small>{money.label}</small>
        </span>
        <ChevronRight className="trow-go" size={16} strokeWidth={1.75} aria-hidden />
      </motion.button>
    </motion.li>
  )
}
