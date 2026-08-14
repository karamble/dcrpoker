import { motion, useReducedMotion } from 'framer-motion'
import { ease } from './motion'
import { usePrevious } from './previous'

// Seats as three states rather than as a fraction.
//
// Joined and committed are different facts and the gap between them is the one
// worth seeing: every seat joined with commits still short is a full-looking
// table some peer has not confirmed, which is what a join that never arrived
// looks like from here.
//
// Three shapes, not three colours - dashed, ring, solid - so the picture still
// says it in greyscale. `commits` is a count and not a per-seat fact, so the
// solid pips are drawn first and no pip claims to be a particular chair.

export function SeatPips({
  seats,
  joined,
  commits,
}: {
  seats: number
  joined: number
  commits: number
}) {
  const reduced = useReducedMotion()
  const was = usePrevious(joined) ?? joined

  return (
    <span
      className="pips"
      role="img"
      aria-label={`${joined} of ${seats} seats taken, ${commits} confirmed`}
    >
      {Array.from({ length: seats }, (_, i) => {
        const kind = i < commits ? 'committed' : i < joined ? 'filled' : 'empty'
        // Only the pip that just filled moves. The rest are told to sit at 1
        // explicitly, or a re-render replays the whole row.
        const fresh = !reduced && i >= was && i < joined
        return (
          <motion.span
            key={i}
            className={`pip-seat ${kind}`}
            initial={false}
            animate={fresh ? { scale: [1, 1.4, 1] } : { scale: 1 }}
            transition={{ duration: 0.34, ease, times: [0, 0.38, 1] }}
          />
        )
      })}
    </span>
  )
}
