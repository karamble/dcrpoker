import { useEffect, useState } from 'react'
import { api, type Asked, type LedgerView, type Snapshot } from '../api'
import { dcr } from '../format'

// The things somebody has to do, and the one thing they have to be told.
//
// Payout addresses are the quiet one. Until *every* seat has said where to pay
// it, no claim at this table can be built at all - so a table where one seat
// never answered has no working punishment for anybody walking out, and until
// this was surfaced the only sign was a claim that never appeared and a
// co-signer refusing for reasons only its own log recorded.
//
// The address itself is injected by the host and never typed here. A page that
// could name where winnings go is a page that could redirect them, so it is not
// something this side is allowed to have an opinion about.

export function Obligations({
  table,
  ledger,
  payoutAddress,
}: {
  table: Snapshot
  ledger?: LedgerView
  payoutAddress?: string
}) {
  const missing = ledger?.payoutsMissing ?? []
  const ours = table.seat
  const oursMissing = ours !== undefined && missing.includes(ours)

  const needsStake = table.seat !== undefined && !table.stake
  const oursBonded =
    ours !== undefined && (ledger?.roster ?? table.roster ?? []).some((s) => s.ours && s.bond)
  const needsBond = table.seat !== undefined && !oursBonded

  const anything = missing.length > 0 || needsStake || needsBond || !ledger?.refreshed

  if (!anything) return null

  return (
    <section className="card">
      <h2>Outstanding</h2>

      {needsStake && (
        <Task
          label={`Pay this seat's stake of ${dcr(table.buyinAtoms)} DCR`}
          note="The table deals when every stake is on the chain. Approving the payment happens in the dashboard behind this panel, not here."
          action="Pay the stake"
          run={() => api.fund(table.sid)}
        />
      )}

      {!needsStake && needsBond && (
        <Task
          label="Post this seat's bond for the table"
          note="A second payment, and a different thing: this is what the seat loses for walking out mid-hand. It is yours again after the lock."
          action="Post the bond"
          run={() => api.bondTable(table.sid)}
        />
      )}

      {missing.length > 0 && (
        <div className="rows">
          <div className="row">
            <span>
              No claim can be built at this table until{' '}
              {missing.length === 1 ? `seat ${missing[0]} says` : `seats ${missing.join(', ')} say`}{' '}
              where to pay it.
            </span>
            <span className="muted">{missing.length} outstanding</span>
          </div>
          {oursMissing && (
            <p className="lede">
              {payoutAddress
                ? 'The host has given this table an address for you; it is announced as soon as the seating is drawn.'
                : 'This is one of them. The address comes from the wallet account you bound to gaming — this page never chooses it.'}
            </p>
          )}
        </div>
      )}

      {table.dealing && !ledger?.refreshed && (
        <p className="lede warn">
          The answers to a future claim are still being gathered. Until every seat holds
          one, a seat claimed against cannot answer — the signatures an answer needs
          include the accusers', and they will not give them once they have started
          accusing.
        </p>
      )}
    </section>
  )
}

/** Task is one thing to do, and what became of asking for it.
 *
 *  The payment is deliberately not waited on here - a person approving one may
 *  take half an hour - so this asks, gets an id, and then follows that id. The
 *  distinction it keeps visible is the one that matters: the host approving a
 *  payment and the plugin having recorded it against a seat are two facts, and
 *  the gap between them is exactly where a payment goes missing. */
function Task({
  label,
  note,
  action,
  run,
}: {
  label: string
  note: string
  action: string
  run: () => Promise<Asked>
}) {
  const [busy, setBusy] = useState(false)
  const [said, setSaid] = useState<string>()
  const [spendId, setSpendId] = useState<string>()

  useEffect(() => {
    if (!spendId) return
    let stopped = false
    const tick = () => {
      api
        .spend(spendId)
        .then((s) => {
          if (stopped) return
          if (s.error) {
            setSaid(s.error)
            setSpendId(undefined)
          } else if (s.recorded) {
            setSaid('paid, and the table has been told')
            setSpendId(undefined)
          } else if (s.txid) {
            setSaid('approved, and being located on the chain')
          } else {
            setSaid(`${s.state} — approve it in the dashboard behind this panel`)
          }
        })
        .catch(() => {
          // Not knowing for a moment is not worth saying anything about.
        })
    }
    tick()
    const id = window.setInterval(tick, 3000)
    return () => {
      stopped = true
      window.clearInterval(id)
    }
  }, [spendId])

  return (
    <div className="rows">
      <div className="row">
        <span>{label}</span>
        <button
          className="act primary"
          disabled={busy || spendId !== undefined}
          onClick={() => {
            setBusy(true)
            setSaid(undefined)
            run()
              .then((asked) => {
                setSpendId(asked.spendId)
                setSaid('asked the host; approve it in the dashboard')
              })
              .catch((e) => setSaid(String(e instanceof Error ? e.message : e)))
              .finally(() => setBusy(false))
          }}
        >
          {busy ? 'asking…' : spendId ? 'waiting…' : action}
        </button>
      </div>
      <p className="lede muted">{note}</p>
      {said && <p className="lede">{said}</p>}
    </div>
  )
}
