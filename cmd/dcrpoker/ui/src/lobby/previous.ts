import { useEffect, useRef } from 'react'

/** usePrevious is the value from the render before this one.
 *
 *  Three components need it and all three need it for the same reason: the
 *  difference is the event. A pip fills when `joined` goes up, the hairline
 *  sweeps when the chain height does, and neither fact is in the snapshot -
 *  only the number is. */
export function usePrevious<T>(value: T): T | undefined {
  const was = useRef<T>()
  useEffect(() => {
    was.current = value
  }, [value])
  return was.current
}
