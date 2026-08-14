import { useState } from 'react'
import { AnimatePresence, motion } from 'framer-motion'
import { ChevronDown, ChevronRight, Lock } from 'lucide-react'
import type { Snapshot } from '../api'
import { dcr } from '../format'
import { go } from '../route'
import { standing } from '../standing'
import { dur, ease } from './motion'
import { who } from './who'

// Tables this player has got up from, kept because they still hold coin.
//
// They are grouped rather than mixed in, which is not a new ordering: finished
// last is already the list's first sort key, drawn as a heading. Grouping is
// what keeps the list readable at twenty rows - a live table is worth two lines
// and a receipt is worth one.
//
// There is no countdown on a receipt, deliberately. The stake is locked for
// csvBlocks from the height it confirmed at, and that height is in neither the
// snapshot nor the ledger; csvBlocks is a duration, not a deadline. A clock
// here would be this page guessing, on a screen whose whole job is to say what
// is actually known.

/** locked is what a finished table is still holding for this player: what the
 *  last signed boundary paid the seat, or the buy-in for one that never dealt.
 *  Undefined when there is no seat to read, which is not zero. */
export function locked(t: Snapshot): number | undefined {
  if (t.seat === undefined) return undefined
  return t.settled?.stacks?.[t.seat] ?? t.buyinAtoms
}

export function Receipts({ tables }: { tables: Snapshot[] }) {
  const [open, setOpen] = useState(tables.length <= 2)
  if (tables.length === 0) return null

  let total = 0
  let partial = false
  for (const t of tables) {
    const a = locked(t)
    if (a === undefined) partial = true
    else total += a
  }

  return (
    <section className="receipts">
      <button
        type="button"
        className="lhead press"
        aria-expanded={open}
        onClick={() => setOpen((was) => !was)}
      >
        <span>
          receipts <span className="count">{tables.length}</span>
        </span>
        <span className="lhead-end">
          <span className="locked">
            <Lock size={16} strokeWidth={1.75} aria-hidden />
            {dcr(total)}
            {partial ? '+' : ''} DCR still locked
          </span>
          {open ? (
            <ChevronDown size={16} strokeWidth={1.75} aria-hidden />
          ) : (
            <ChevronRight size={16} strokeWidth={1.75} aria-hidden />
          )}
        </span>
      </button>

      <AnimatePresence initial={false}>
        {open && (
          <motion.div
            className="receipt-list"
            initial={{ height: 0, opacity: 0 }}
            animate={{ height: 'auto', opacity: 1, transition: { duration: dur.base, ease } }}
            exit={{ height: 0, opacity: 0, transition: { duration: dur.quick, ease } }}
          >
            {tables.map((t) => (
              <Receipt key={t.sid} table={t} />
            ))}
            <p className="lede muted">
              Each of these is coin of yours still on the chain. They go when the chain says
              the coin is gone, which is not a date this game can work out for you.
            </p>
          </motion.div>
        )}
      </AnimatePresence>
    </section>
  )
}

function Receipt({ table }: { table: Snapshot }) {
  const it = standing(table)
  const held = locked(table)

  return (
    <button
      type="button"
      className="receipt"
      onClick={() => go({ view: 'table', sid: table.sid, tab: 'audit' })}
      aria-label={`${who(table)}. ${it.say}. ${held !== undefined ? `${dcr(held)} DCR locked.` : ''}`}
    >
      <span className="mono">{table.sid.slice(0, 8)}</span>
      <span className="receipt-who">{who(table)}</span>
      <span className="muted">{it.say}</span>
      <span className="locked">
        <Lock size={16} strokeWidth={1.75} aria-hidden />
        {held !== undefined ? `${dcr(held)} DCR` : 'no seat to read'}
      </span>
      <ChevronRight className="trow-go" size={16} strokeWidth={1.75} aria-hidden />
    </button>
  )
}
