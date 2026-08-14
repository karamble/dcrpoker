import type { Transition, Variants } from 'framer-motion'

// One curve, four durations, two springs.
//
// Kept in one place because a screen where every element eases differently
// reads as several screens. The curve is the one the felt already animates
// cards and chips on, so the lobby and the table move the same way.
//
// Nothing here is gated on reduced motion. MotionConfig at the root handles
// that for transforms and layout; the few things it cannot reach - a value
// counting up, a pip flashing - ask useReducedMotion where they are used.

export const ease: [number, number, number, number] = [0.2, 0.8, 0.3, 1]

export const dur = { quick: 0.16, base: 0.24, enter: 0.28 }

/** press is stiff and short. A button that springs is a button that feels
 *  late. */
export const press: Transition = { type: 'spring', stiffness: 520, damping: 38, mass: 0.9 }

/** shift is what a row uses when a neighbour appears and it has to move. */
export const shift: Transition = { type: 'spring', stiffness: 340, damping: 34, mass: 1 }

/** The stagger is capped at ten, so twenty tables do not take a second to land,
 *  and it is passed as `custom` only on the first paint - a table arriving at
 *  minute forty must not wait 250ms for being tenth. */
export const trow: Variants = {
  initial: { opacity: 0, y: -8, scale: 0.985 },
  animate: (i: number = 0) => ({
    opacity: 1,
    y: 0,
    scale: 1,
    transition: { duration: dur.enter, ease, delay: Math.min(i, 9) * 0.028 },
  }),
  exit: { opacity: 0, scale: 0.97, transition: { duration: dur.base, ease } },
}
