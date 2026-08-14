import { useEffect, useState } from 'react'
import { motion, useReducedMotion } from 'framer-motion'
import { usePrevious } from './previous'

// A hairline that sweeps once when the chain moves.
//
// Every deadline in this game is a block height, so the block arriving is the
// only clock there is to draw. Blocks are about five minutes apart, which makes
// this the rarest motion on the screen and the only one that says the whole
// thing runs off a chain rather than off this machine.
//
// The height itself is printed beside it. The sweep decorates a fact; it is
// not one.

export function BlockTick({ height }: { height?: number }) {
  const reduced = useReducedMotion()
  const was = usePrevious(height)
  const [run, setRun] = useState<number>()

  useEffect(() => {
    if (reduced || height === undefined || was === undefined || height <= was) return
    setRun(height)
    const id = window.setTimeout(() => setRun(undefined), 950)
    return () => window.clearTimeout(id)
  }, [height, was, reduced])

  return (
    <div className="tick" aria-hidden>
      {run !== undefined && (
        <motion.span
          key={run}
          className="tick-run"
          style={{ originX: 0 }}
          initial={{ scaleX: 0, opacity: 1 }}
          animate={{ scaleX: 1, opacity: 0 }}
          transition={{ duration: 0.9, ease: [0.3, 0, 0.2, 1] }}
        />
      )}
    </div>
  )
}
