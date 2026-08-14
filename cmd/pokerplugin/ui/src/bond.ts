import { useCallback, useEffect, useState } from 'react'
import { api, type Bond as BondInfo } from './api'

// One clock for the standing bond.
//
// The fetch is hoisted out of <Bond/> rather than the value, and the difference
// matters under tabs: the card is only mounted on the Money tab, so a value it
// owned would be undefined everywhere else - and Progress's first step reads
// `bond ? bond.hasDeposit : true`, which would report a bond that may not exist
// as posted. Asking here means the answer is the same wherever it is read.

export type BondState = { bond?: BondInfo; error?: string; reload: () => void }

export function useBond(): BondState {
  const [bond, setBond] = useState<BondInfo>()
  const [error, setError] = useState<string>()

  const reload = useCallback(() => {
    api
      .bond()
      .then((b) => {
        setBond(b)
        setError(undefined)
      })
      .catch((e) => setError(String(e instanceof Error ? e.message : e)))
  }, [])

  useEffect(() => {
    reload()
    // Slowly. A lock measured in thousands of blocks does not need watching,
    // and the only thing that moves here is the chain.
    const id = window.setInterval(reload, 60_000)
    return () => window.clearInterval(id)
  }, [reload])

  return { bond, error, reload }
}
