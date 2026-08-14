import type { Transition, Variants } from 'framer-motion'

// How the table moves, with the numbers in one file.
//
// They have to agree with each other more than any one of them has to be right
// on its own: a chip that reaches the pot after the pot has already bumped
// reads as two unrelated events rather than one.
//
// Nothing here asserts anything. A card animates because it opened, a chip
// because a street ended, the button because the log says it moved. Stillness
// already says all of it, which MotionConfig reducedMotion="user" at the app
// root makes literally true - it drops the transforms and keeps the result.

/** The felt's own curve. Quick out, settled, no bounce: chips are clay. */
export const settle: Transition = { type: 'spring', stiffness: 520, damping: 38, mass: 0.7 }

/** Chips crossing the felt. Slower, because weight reads as time. */
export const travel: Transition = { type: 'spring', stiffness: 340, damping: 34, mass: 0.9 }

/** A chip leaving for the pot. Not a spring: it is going somewhere and then it
 *  is gone, and a spring that settles onto a target it fades out of is motion
 *  nobody sees the end of. */
export const sweep: Transition = { duration: 0.46, ease: [0.4, 0, 0.6, 1] }

/** A card dealt from the middle of the table to a seat, given the offset in
 *  pixels it has to cover and its place in the row. */
export const dealt = (dx: number, dy: number, i: number): Variants => ({
  from: { x: dx, y: dy, rotate: -7 + i * 5, opacity: 0, scale: 0.88 },
  at: {
    x: 0,
    y: 0,
    rotate: 0,
    opacity: 1,
    scale: 1,
    transition: { ...settle, delay: i * 0.06 },
  },
})
