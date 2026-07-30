import { useState } from 'react'
import { api, type Snapshot } from '../api'
import { blocks, dcr } from '../format'

// How far along this table is, in the protocol's own vocabulary.
//
// The states come straight from the formation machine, and they are not
// renamed: "committed" and "settled" mean particular things here and inventing
// friendlier words would make the state a caller sees and the state the
// protocol is in two different things.
//
// Deadlines are block heights, because a deadline both parties can check has to
// be a fact about the chain and not about anybody's clock. A height on its own
// is useless to a person, so it is always shown next to how far away it is.

const explain: Record<Snapshot['state'], string> = {
  joining: 'Collecting players. Nothing is bound and nothing has been paid.',
  formed: 'Enough seats have answered. The membership can be computed but nobody is bound yet.',
  committed: 'This peer has bound itself to a membership and is waiting for the others.',
  settled: 'Every seat is bound to the same membership. This is the roster the money is tied to.',
  aborted: 'This table ended before it dealt.',
  unknown: 'This table is in a state this build does not recognise.',
}

export function Lifecycle({ table }: { table: Snapshot }) {
  return (
    <section className="card">
      <h2>The table</h2>
      <p className="headline">
        <span className="pill">{table.state}</span>{' '}
        {table.dealing ? (table.over ? 'and finished' : 'and dealing') : ''}
      </p>
      <p className="lede">{explain[table.state] ?? explain.unknown}</p>
      {table.reason && <p className="lede warn">{table.reason}</p>}
      {(table.state === 'formed' || table.state === 'committed') &&
        table.joined >= table.seats &&
        table.commits < table.seats && (
          <p className="lede warn">
            Every seat has joined; the roster is not yet confirmed by all of
            them. If this does not resolve within a block or two, the other
            side may never have received a join.
          </p>
        )}

      <div className="rows">
        <Row label="Seats" value={`${table.joined} of ${table.seats} joined`} />
        {(table.state === 'formed' || table.state === 'committed') && (
          <Row
            label="Commitments"
            value={`${table.commits} of ${table.seats} confirmed`}
          />
        )}
        <Row label="Buy-in" value={`${dcr(table.buyinAtoms)} DCR`} />
        <Row label="Stakes on chain" value={`${table.funded} of ${table.seats}`} />
        <Row label="Bonds on chain" value={`${table.bonded} of ${table.seats}`} />
        {table.hand ? <Row label="Hand" value={String(table.hand)} /> : null}
        {table.waiting > 0 && (
          <Row
            label="Log entries out of order"
            value={`${table.waiting} waiting on one before them`}
          />
        )}
        {table.fundingDeadline ? (
          <Row
            label="Funding deadline"
            value={`block ${table.fundingDeadline.toLocaleString()} · ${blocks(
              table.fundingDeadline,
              table.height,
            )}`}
          />
        ) : null}
        {table.depositAddr && <Row label="Your stake is paid to" value={table.depositAddr} />}
        {table.matchId && <Row label="Match" value={table.matchId} />}
      </div>

      {table.funded === table.seats && table.bonded < table.seats && (
        <p className="lede">
          Every stake is down and not every bond is. The table waits for both: dealing
          without them would mean playing with no answer to somebody who stops.
        </p>
      )}

      {!table.finished && <Leave table={table} />}
    </section>
  )
}

/** Leave is how somebody gets up, and it is deliberately not the same thing as
 *  closing the panel.
 *
 *  Walking out mid-hand is what the bond answers; this is the other thing. It
 *  says so on the wire, folds this seat when its turn comes, and the table ends
 *  at the next hand boundary - never mid-hand, because dissolving in the middle
 *  would hand whoever was losing a way out of the pot.
 *
 *  Two presses rather than a dialog. The second press is the confirmation, and
 *  it is worth having because this is not undoable: a seat that has got up
 *  cannot sit back down, since the roster, the escrow and the bond all name the
 *  full membership. */
function Leave({ table }: { table: Snapshot }) {
  const [asked, setAsked] = useState(false)
  const [busy, setBusy] = useState(false)
  const [said, setSaid] = useState<string>()
  const [error, setError] = useState<string>()

  const mine = table.roster?.find((s) => s.ours)
  if (mine?.leaving) {
    return (
      <p className="lede">
        You have said you are getting up. The table ends at the next hand boundary and
        pays out at the last result every seat signed.
      </p>
    )
  }

  return (
    <div className="rows">
      <div className="row">
        <span>
          {asked
            ? 'This cannot be undone — press again to get up'
            : table.dealing
              ? 'Get up from this table'
              : 'Leave this table'}
        </span>
        <button
          className={asked ? 'act primary' : 'act'}
          disabled={busy}
          onClick={() => {
            if (!asked) {
              setAsked(true)
              return
            }
            setBusy(true)
            setError(undefined)
            api
              .leave(table.sid)
              .then((r) =>
                setSaid(
                  r.left
                    ? 'Said. The table ends at the next hand boundary.'
                    : 'This player is not at that table.',
                ),
              )
              .catch((e) => setError(String(e instanceof Error ? e.message : e)))
              .finally(() => setBusy(false))
          }}
        >
          {busy ? 'saying…' : asked ? 'Yes, get up' : 'Leave'}
        </button>
      </div>
      <p className="lede muted">
        {table.dealing
          ? 'Your hand is folded when its turn comes and the table settles at the boundary after that. Your stake and bond are paid back on the chain; nothing is forfeited by leaving this way.'
          : 'Nothing has been dealt, so this only gives up the seat.'}
      </p>
      {said && <p className="lede">{said}</p>}
      {error && <p className="lede bad">{error}</p>}
    </div>
  )
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="row">
      <span>{label}</span>
      <span title={value}>{value}</span>
    </div>
  )
}
