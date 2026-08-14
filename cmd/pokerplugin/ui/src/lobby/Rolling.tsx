import { useEffect } from 'react'
import { motion, useMotionValue, useReducedMotion, useSpring, useTransform } from 'framer-motion'
import { dcr } from '../format'

// A number that arrives rather than replaces itself.
//
// Totals only. An amount somebody is being asked to agree to is written once
// and does not move: a figure still settling while a person decides whether to
// pay it has been made harder to read in exchange for looking nice. What rolls
// here is what the tables add up to, which nobody presses.

export function Rolling({ atoms, fromZero }: { atoms: number; fromZero?: boolean }) {
  const reduced = useReducedMotion()
  // restDelta is one atom. Below that the spring is arguing about a digit
  // dcr() rounds away anyway.
  const raw = useMotionValue(fromZero ? 0 : atoms)
  const eased = useSpring(raw, { stiffness: 120, damping: 26, restDelta: 1 })
  const text = useTransform(eased, (v) => dcr(Math.round(v)))

  useEffect(() => {
    raw.set(atoms)
  }, [atoms, raw])

  // Reduced motion gets the value, not a slower version of the animation.
  if (reduced) return <>{dcr(atoms)}</>
  return <motion.span>{text}</motion.span>
}
