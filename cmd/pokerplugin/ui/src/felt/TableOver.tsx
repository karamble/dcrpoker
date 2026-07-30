import type { LedgerView, Snapshot } from '../api'
import { dcr, short } from '../format'

// The end of a table, told properly.
//
// A table that ends is the interface's last and most important moment: money is
// leaving escrow and going to somebody, and the screen used to say nothing -
// worse, it said a between-hands sentence about waiting for cards, forever,
// while the settlement sat in the mempool. This overlay says what actually
// happened and what is actually happening: the overall result across the whole
// table, the payout as it gathers signatures and reaches the network with its
// transaction id, and the bonds coming home.
//
// Everything here is read from the ledger and the chain events the plugin
// already reports - the signatures counted, the txids as broadcast. Nothing is
// invented and nothing is a placeholder: if a line cannot be said from data, it
// is not said.

export function TableOver({
  table,
  ledger,
  onDetails,
  onClose,
}: {
  table: Snapshot
  ledger?: LedgerView
  onDetails: () => void
  onClose: () => void
}) {
  // The overall result: what the last signed boundary left this seat against
  // what it bought in for. The whole table's story, not the last hand's.
  const mySeat = table.seat
  const final = mySeat !== undefined ? table.settled?.stacks?.[mySeat] : undefined
  const net = final !== undefined ? final - table.buyinAtoms : undefined

  const settlement = ledger?.settlement
  const disputes = ledger?.disputes ?? []
  const events = ledger?.events ?? []
  const paidOut = events.find((e) => e.kind === 'settled' && !e.text.includes('bond'))
  const bondsBack = events.filter((e) => e.kind === 'settled' && e.text.includes('bond'))

  const payoutLine = () => {
    if (paidOut?.txid) {
      return (
        <div className="end-row">
          <span>Winner paid in</span>
          <b className="mono" title={paidOut.txid}>
            {short(paidOut.txid)}
          </b>
        </div>
      )
    }
    if (settlement?.done) {
      return (
        <div className="end-row">
          <span>Payout</span>
          <b className="good">sent</b>
        </div>
      )
    }
    if (settlement) {
      return (
        <div className="end-row">
          <span>Signing the payout</span>
          <b>
            {settlement.signed?.length ?? 0} of {settlement.needs}
          </b>
        </div>
      )
    }
    return (
      <div className="end-row">
        <span>Payout</span>
        <b>being prepared</b>
      </div>
    )
  }

  return (
    <div className="stage-over">
      <div className="endcard">
        <h2>Table finished</h2>

        <p className={`end-result${net !== undefined && net > 0 ? ' won' : net !== undefined && net < 0 ? ' lost' : ''}`}>
          {net === undefined
            ? 'The last signed result decides the payout.'
            : net > 0
              ? `You won ${dcr(net)} DCR`
              : net < 0
                ? `You lost ${dcr(-net)} DCR`
                : 'You broke even'}
        </p>

        {disputes.map((d) => (
          <p className="end-result lost" key={`d${d.hand}`}>
            {d.named === mySeat
              ? `Hand ${d.hand} ended in a dispute that named you`
              : `Hand ${d.hand}'s shuffle was disputed — seat ${d.named} ${
                  d.named === d.shuffler ? 'signed a shuffle that does not verify' : 'made a false complaint'
                }`}
            . The hand is void: the table pays the last result every seat signed, and
            seat {d.named}&apos;s bond stays held.
          </p>
        ))}

        {settlement?.pays && settlement.pays.length > 0 && (
          <div className="end-rows">
            {settlement.pays.map((p) => (
              <div className="end-row" key={p.seat}>
                <span>
                  seat {p.seat}
                  {p.seat === mySeat ? ' · you' : ''} receives
                </span>
                <b>{dcr(p.atoms)} DCR</b>
              </div>
            ))}
          </div>
        )}

        <div className="end-rows">
          {payoutLine()}
          {settlement?.blocked && (
            <div className="end-row">
              <span>Not yet, because</span>
              <b className="warn">{settlement.blocked}</b>
            </div>
          )}
          {bondsBack.map((e, i) => (
            <div className="end-row" key={i}>
              <span>
                {e.seat !== undefined ? `Seat ${e.seat}'s bond` : 'A bond'} came home
                {e.seat === mySeat ? ' · yours' : ''}
              </span>
              <b className="mono" title={e.txid}>
                {e.txid ? short(e.txid) : '—'}
              </b>
            </div>
          ))}
        </div>

        <p className="end-note">
          The payout spends the table's escrow, which needs every seat's signature —
          nobody could have built it alone. Once it confirms, this table is only a
          record.
        </p>

        <div className="end-actions">
          <button className="act" onClick={onDetails}>
            Details
          </button>
          <button className="act primary" onClick={onClose}>
            Close
          </button>
        </div>
      </div>
    </div>
  )
}
