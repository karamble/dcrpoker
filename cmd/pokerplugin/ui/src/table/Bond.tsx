import { useState } from 'react'
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
// A presenter. The fetch and its clock live in bond.ts, because the step rail
// asks whether this player can join anything at all and this card says when the
// bond comes back - and this card is only mounted on one tab, so a value it
// owned would be missing everywhere else.
//
// Reclaiming it is not offered here. There is no route for it on the plugin's
// mux: it builds and broadcasts a transaction this seat signs alone, and the
// place that already does that is the dashboard. Where to do it is said instead
// of shown.

export function Bond({
  bond,
  error,
  reload,
}: {
  bond?: BondInfo
  error?: string
  reload: () => void
}) {
  const [asked, setAsked] = useState<Asked>()
  const [busy, setBusy] = useState(false)
  const [said, setSaid] = useState<string>()

  if (!bond) {
    return (
      <section className="card">
        <h2>Your bond</h2>
        <p className="lede">{error ?? 'Asking…'}</p>
        {error && (
          <div className="actions">
            <button className="act" onClick={reload}>
              Ask again
            </button>
          </div>
        )}
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
                setSaid(undefined)
                api
                  .fundBond()
                  .then((a) => setAsked(a))
                  .catch((e) => setSaid(String(e instanceof Error ? e.message : e)))
                  .finally(() => setBusy(false))
              }}
            >
              {busy ? 'asking…' : asked ? 'waiting…' : 'Post the bond'}
            </button>
          </div>
        </div>
        {asked && (
          <p className="lede">
            Asked the host. Approve the payment in the dashboard; it may sit there for a
            while, and nothing here is waiting on it.
          </p>
        )}
        {(said ?? error) && <p className="lede bad">{said ?? error}</p>}
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
          ? 'Taking it back is done from the dashboard, in Bison Relay > Gaming. This game has no route for it: it signs and broadcasts a transaction alone, and the dashboard is where that already happens.'
          : 'Until the lock matures nothing can move it, including this player. Taking it back afterwards is done from the dashboard, in Bison Relay > Gaming.'}
      </p>
      {bond.chainErr && (
        <p className="lede warn">
          The chain could not be asked about it just now, so the numbers above may be stale.
          The bond is where it says either way.
        </p>
      )}
      {(said ?? error) && <p className="lede bad">{said ?? error}</p>}
    </section>
  )
}
