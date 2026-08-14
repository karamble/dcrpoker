import { AnimatePresence, motion } from 'framer-motion'
import { TriangleAlert } from 'lucide-react'
import type { HandView, Snapshot } from '../api'
import { go } from '../route'
import { standing, type Standing } from '../standing'
import { dur, ease } from './motion'
import { who } from './who'

// A table is waiting on this player, and it may be below the fold.
//
// The list must not reorder to say so - it is sorted the same way on both sides
// of the wire precisely so a row does not move under a cursor - so the urgency
// goes here instead, above it, where it costs the order nothing.
//
// This is the lobby's whole answer to the owes alarm. The alarm in the shell is
// driven from a table view and there is no sid at the lobby, so its roster is
// empty here and it renders nothing; a duty standing against this seat is
// exactly the case that costs money, and without this the lobby would be the
// one screen that never mentions it. It also covers the pull to the felt being
// deliberately one-shot: somebody who walked back during a hand is not dragged
// forward again, and this is what that rule owes them.

/** The order to speak in when more than one table wants something. Ranking what
 *  is said is not reordering what is listed - the rows below do not move. */
const rank: Record<Standing['tone'], number> = { bad: 0, hot: 1, owed: 2, waiting: 3, cold: 4 }

const where: Record<Standing['goto'], string> = {
  felt: 'Go to the felt',
  money: 'Open the money',
  now: 'Open the table',
  roster: 'Open the table',
  audit: 'Open the record',
}

export function WaitingOnYou({
  tables,
  hands,
}: {
  tables: Snapshot[]
  hands: Record<string, HandView>
}) {
  const wanted: { table: Snapshot; it: Standing }[] = []
  for (const t of tables) {
    if (t.finished) continue
    const it = standing(t, hands[t.sid])
    if (it.yours) wanted.push({ table: t, it })
  }
  wanted.sort((a, b) => rank[a.it.tone] - rank[b.it.tone])

  const first = wanted[0]

  return (
    <AnimatePresence initial={false}>
      {first && (
        <motion.div
          className={`callout ${first.it.tone}`}
          role="status"
          aria-live="polite"
          initial={{ height: 0, opacity: 0 }}
          animate={{ height: 'auto', opacity: 1, transition: { duration: dur.base, ease } }}
          exit={{ height: 0, opacity: 0, transition: { duration: dur.quick, ease } }}
        >
          {first.it.tone === 'bad' && <TriangleAlert size={16} strokeWidth={1.75} aria-hidden />}
          <span className="callout-say">
            <b>{first.it.say}</b> at {who(first.table)}
            {first.it.note ? ` — ${first.it.note}` : ''}
            {wanted.length > 1 ? ` (and ${wanted.length - 1} more)` : ''}
          </span>
          <span className="mast-spacer" />
          <button
            type="button"
            className="act primary"
            onClick={() =>
              go(
                first.it.goto === 'felt'
                  ? { view: 'felt', sid: first.table.sid }
                  : { view: 'table', sid: first.table.sid, tab: first.it.goto },
              )
            }
          >
            {where[first.it.goto]}
          </button>
        </motion.div>
      )}
    </AnimatePresence>
  )
}
