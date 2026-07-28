import type { LedgerView, Snapshot } from '../api'
import { dcr, delta, short } from '../format'

// Where the money is.
//
// Two numbers per seat and they mean different things, which is the single most
// important distinction on this screen:
//
//   Signed - the last hand boundary every seat put a signature to. If this
//            table stopped right now, this is what it pays out. It is a fact
//            on which everybody has already agreed.
//   Now    - what the table currently thinks. It is a promise. Nothing has
//            been signed for it, and a hand that never finishes voids back to
//            Signed.
//
// Showing only one of them would be a lie in either direction: only Now, and a
// player believes money is theirs that nobody has agreed to; only Signed, and
// the hand they are playing appears not to be happening.
//
// The stake and bond columns are the same discipline applied to the chain. An
// empty outpoint is rendered as "not seen by this peer", never as "unfunded" -
// the plugin's funded count is deliberately what this process checked for
// itself, and rendering an absence as a fact about somebody else would throw
// that away at the last step.

export function Money({ table, ledger }: { table: Snapshot; ledger?: LedgerView }) {
  const settled = ledger?.settled ?? table.settled
  const live = ledger?.live ?? table.live
  const roster = ledger?.roster ?? table.roster ?? []

  if (!settled && !live) {
    return (
      <section className="card">
        <h2>The money</h2>
        <p className="lede">
          Nothing has been dealt, so every seat still holds its buy-in of{' '}
          <span className="mono">{dcr(table.buyinAtoms)}</span> DCR.
        </p>
        <Chain roster={roster} />
      </section>
    )
  }

  const voids = table.hand && settled && table.hand > settled.hand

  return (
    <section className="card">
      <h2>The money</h2>
      {voids && (
        <p className="lede">
          If this table stopped now, hand {table.hand} is void and it settles at hand{' '}
          {settled?.hand === 0 ? 'zero, the buy-ins' : settled?.hand}.
        </p>
      )}
      <table className="grid">
        <thead>
          <tr>
            <th>Seat</th>
            <th className="num">Signed</th>
            <th className="num">Now</th>
            <th className="num">Difference</th>
          </tr>
        </thead>
        <tbody>
          {roster.map((s) => {
            const was = settled?.stacks?.[s.seat]
            const is = live?.[s.seat]
            return (
              <tr key={s.seat} className={s.ours ? 'ours' : undefined}>
                <td>
                  seat {s.seat}
                  {s.ours && <span className="muted"> · you</span>}
                </td>
                <td className="num">{dcr(was)}</td>
                <td className="num">{dcr(is)}</td>
                <td className="num muted">
                  {was !== undefined && is !== undefined ? delta(is - was) : '—'}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
      <p className="lede">
        <strong>Signed</strong> is the last boundary every seat put their name to
        {settled ? ` (hand ${settled.hand})` : ''}. <strong>Now</strong> is what the table
        thinks — a promise, not something anybody has agreed to.
      </p>
      <Chain roster={roster} />
    </section>
  )
}

function Chain({ roster }: { roster: LedgerView['roster'] }) {
  if (!roster || roster.length === 0) return null
  return (
    <>
      <table className="grid">
        <thead>
          <tr>
            <th>Seat</th>
            <th>Stake</th>
            <th>Bond</th>
          </tr>
        </thead>
        <tbody>
          {roster.map((s) => (
            <tr key={s.seat} className={s.ours ? 'ours' : undefined}>
              <td>
                seat {s.seat}
                {s.ours && <span className="muted"> · you</span>}
              </td>
              <td>
                {s.stake ? (
                  <span className="mono" title={s.stake}>
                    {short(s.stake)}
                  </span>
                ) : (
                  <span className="muted">not seen by this peer</span>
                )}
              </td>
              <td>
                {s.bondAt || s.bond ? (
                  <span className="mono" title={s.bondAt || s.bond}>
                    {short(s.bondAt || s.bond)}
                    {s.bondAt && <span className="muted"> · moved</span>}
                  </span>
                ) : (
                  <span className="muted">not seen by this peer</span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="lede muted">
        These are outputs this peer found on the chain itself, not announcements it was
        given. An empty one means it has not seen the money — which is not the same as the
        money not being there.
      </p>
    </>
  )
}
