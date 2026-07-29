import { useCallback, useEffect, useState } from 'react'
import { api, type Asked, type Bond as BondInfo } from '../api'
import { blocks, dcr, short } from '../format'

// The standing bond, which is this player's and not any table's.
//
// It is posted once, buys the right to join anything, and is deliberately not
// forfeitable - at registration there is no roster to name, so there is nobody
// it could be forfeited to. What it does is make an identity cost something,
// which matters because keys are free to generate.
//
// It is shown whether or not this player is at a table, because that is what it
// is: coin locked for two weeks with nothing on screen to say so. The one
// question anybody has about it is when it comes back, and that is not a
// property of the deposit but of how many blocks now sit on top of it.
//
// Reclaiming it is not offered here. It builds and broadcasts a transaction
// this seat signs alone, and this page runs in a sandboxed frame with an
// allowlisted API - a page that could cause a broadcast is a larger thing than
// a page that can read. Where to do it is said instead of shown.

export function Bond({ onLoad }: { onLoad?: (b: BondInfo) => void }) {
  const [bond, setBond] = useState<BondInfo>()
  const [error, setError] = useState<string>()
  const [asked, setAsked] = useState<Asked>()
  const [busy, setBusy] = useState(false)

  const load = useCallback(() => {
    api
      .bond()
      .then((b) => {
        setBond(b)
        onLoad?.(b)
        setError(undefined)
      })
      .catch((e) => setError(String(e instanceof Error ? e.message : e)))
  }, [onLoad])

  useEffect(() => {
    load()
    // Slowly. A lock measured in thousands of blocks does not need watching,
    // and the only thing that moves here is the chain.
    const id = window.setInterval(load, 60_000)
    return () => window.clearInterval(id)
  }, [load])

  if (!bond) {
    return (
      <section className="card">
        <h2>Your bond</h2>
        <p className="lede">{error ?? 'Asking…'}</p>
      </section>
    )
  }

  if (!bond.hasDeposit) {
    return (
      <section className="card">
        <h2>Your bond</h2>
        <p className="headline">No bond posted, so this player can join nothing.</p>
        <p className="lede">
          A seat has to cost something or a table can be filled, or blocked, by keys that
          cost nothing to make. It is {dcr(bond.minAtoms)} DCR, locked for{' '}
          {bond.minBlocks.toLocaleString()} blocks, and it is yours again after that. It is
          never forfeitable: there is no table for it to be forfeited to.
        </p>
        <div className="rows">
          <div className="row">
            <span>Paid to</span>
            <span className="mono">{bond.address}</span>
          </div>
          <div className="row">
            <span>Post it</span>
            <button
              className="act primary"
              disabled={busy || asked !== undefined}
              onClick={() => {
                setBusy(true)
                setError(undefined)
                api
                  .fundBond()
                  .then((a) => setAsked(a))
                  .catch((e) => setError(String(e instanceof Error ? e.message : e)))
                  .finally(() => setBusy(false))
              }}
            >
              {busy ? 'asking…' : asked ? 'waiting…' : 'Post the bond'}
            </button>
          </div>
        </div>
        {asked && (
          <p className="lede">
            Asked the host. Approve the payment in the dashboard behind this panel; it may
            sit there for a while, and nothing here is waiting on it.
          </p>
        )}
        {error && <p className="lede bad">{error}</p>}
      </section>
    )
  }

  if (bond.spent) {
    return (
      <section className="card">
        <h2>Your bond</h2>
        <p className="headline">Nothing is locked at {short(bond.outpoint)}.</p>
        <p className="lede">
          That output holds no coin anyone can see. Either it was never confirmed, or it has
          already been taken back.
        </p>
      </section>
    )
  }

  const ready = bond.spendable === true

  return (
    <section className="card">
      <h2>Your bond</h2>
      <p className="headline">
        {dcr(bond.atoms ?? bond.minAtoms)} DCR locked
        {ready ? ', and free to take back' : ''}.
      </p>
      <div className="rows">
        <div className="row">
          <span>Held at</span>
          <span className="mono" title={bond.outpoint}>
            {short(bond.outpoint)}
          </span>
        </div>
        <div className="row">
          <span>Address</span>
          <span className="mono">{bond.address}</span>
        </div>
        <div className="row">
          <span>Lock</span>
          <span>{bond.minBlocks.toLocaleString()} blocks</span>
        </div>
        {bond.confirmations !== undefined && (
          <div className="row">
            <span>Confirmations</span>
            <span>{bond.confirmations.toLocaleString()}</span>
          </div>
        )}
        {bond.maturesAt !== undefined && (
          <div className="row">
            <span>{ready ? 'Was free from' : 'Free from'}</span>
            <span>
              block {bond.maturesAt.toLocaleString()}
              {!ready && bond.blocksLeft ? ` · ${blocks(bond.maturesAt, bond.height)}` : ''}
            </span>
          </div>
        )}
      </div>
      <p className="lede muted">
        {ready
          ? 'Taking it back is done from the dashboard, in Bison Relay > Gaming. It signs and broadcasts a transaction, which is not something a game’s own page is allowed to do.'
          : 'Until the lock matures nothing can move it, including this player. Taking it back afterwards is done from the dashboard, in Bison Relay > Gaming.'}
      </p>
      {bond.chainErr && (
        <p className="lede warn">
          The chain could not be asked about it just now, so the numbers above may be stale.
          The bond is where it says either way.
        </p>
      )}
      {error && <p className="lede bad">{error}</p>}
    </section>
  )
}
