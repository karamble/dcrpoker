import { useEffect, useState } from 'react'
import { api, type Asked, type Bond as BondInfo, type LedgerView, type Snapshot } from '../api'
import { blocks, dcr } from '../format'
import { Awaited } from './Money'

// Where this table has got to, and what - if anything - to press.
//
// Joining a table is seven things in a fixed order and only three of them ask
// anything of a person. Before this the screen said all of it at once, in six
// cards of equal weight, and left somebody to work out which one was waiting on
// them. That is a poor way to present anything and a bad way to present the part
// where money moves: at the first live table a peer read "not seen by this peer"
// for minutes while a stake sat in the mempool, which is exactly the state in
// which somebody pays a second time.
//
// Two rules the rest of this file follows.
//
// A step with nothing to press must say so. Three of the seven are waits - for
// other players, for a block, for somebody else's coin to confirm - and a wait
// rendered as an unfinished task reads as a stall. They say what is being waited
// on and roughly how long it takes.
//
// And the payment steps say where the approval happens, which is not here. This
// page runs in a sandboxed frame and can no more take a passphrase than it can
// sign; it asks the host, and a person approves it in the dashboard. Saying so
// is the honest version of a progress bar that would otherwise appear to stop.

type StepState = 'done' | 'now' | 'later'

type Step = {
  key: string
  short: string
  title: string
  done: boolean
  /** blocked is true when the step cannot even be attempted yet, which is not
   *  the same as not being done. A seat cannot pay a stake before it has a
   *  seat, and saying "pay your stake" then would be asking for something
   *  impossible. */
  body: (s: StepState) => React.ReactNode
}

export function Progress({
  table,
  ledger,
  bond,
  onFelt,
}: {
  table: Snapshot
  ledger?: LedgerView
  bond?: BondInfo
  onFelt: () => void
}) {
  const roster = ledger?.roster ?? table.roster ?? []
  const ours = roster.find((s) => s.ours)
  const seated = table.seat !== undefined
  const missing = ledger?.payoutsMissing ?? []

  const steps: Step[] = [
    {
      key: 'bond',
      short: 'bond',
      title: 'Post a bond',
      // Undefined while the answer has not arrived. Treated as done rather
      // than as missing, because this player plainly did join something.
      done: bond ? bond.hasDeposit : true,
      body: () => (
        <>
          <p className="lede">
            A seat has to cost something, or a table can be filled - or blocked - by keys
            that cost nothing to make. It is locked for a while and is yours again after;
            it is never forfeitable.
          </p>
          {bond && !bond.hasDeposit && (
            <Task
              label={`Post the ${dcr(bond.minAtoms)} DCR bond`}
              note="It buys the right to join anything, once."
              action="Post the bond"
              run={() => api.fundBond()}
            />
          )}
        </>
      ),
    },
    {
      key: 'seats',
      short: 'seats',
      title: 'Wait for the seats to fill',
      done: table.state === 'settled',
      body: () => (
        <>
          <p className="lede">
            {table.joined} of {table.seats} players have joined. The table forms when the
            last one does, and every seat works the membership out for itself rather than
            being told it.
          </p>
          {table.until ? (
            <Row label="Nobody may join after" value={`block ${table.until.toLocaleString()} · ${blocks(table.until, table.height)}`} />
          ) : null}
          {table.reason && <p className="lede warn">{table.reason}</p>}
        </>
      ),
    },
    {
      key: 'draw',
      short: 'draw',
      title: 'Wait for the seating to be drawn',
      done: seated,
      body: () => (
        <>
          <p className="lede">
            Seats come from a block, so that nobody - including whoever made the table -
            could have known the order while there was still time to pick a key for it.
            There is nothing to do here and no way to hurry it.
          </p>
          {table.seatsAt ? (
            <Row label="Drawn from" value={`block ${table.seatsAt.toLocaleString()} · ${blocks(table.seatsAt, table.height)}`} />
          ) : null}
        </>
      ),
    },
    {
      key: 'stake',
      short: 'stake',
      title: 'Pay your stake',
      done: Boolean(ours?.stake),
      body: () => (
        <>
          {!seated ? (
            <p className="lede">This waits for a seat to pay into.</p>
          ) : (
            <>
              <Task
                label={`Pay this seat's stake of ${dcr(table.buyinAtoms)} DCR`}
                note="This is what you play with. It is refundable by you alone after the timelock, and it is what the table pays out of at the end."
                action="Pay the stake"
                run={() => api.fund(table.sid)}
              />
              {table.depositAddr && <Row label="Paid to" value={table.depositAddr} />}
              {table.fundingDeadline ? (
                <Row
                  label="Every seat must be paid for by"
                  value={`block ${table.fundingDeadline.toLocaleString()} · ${blocks(table.fundingDeadline, table.height)}`}
                />
              ) : null}
            </>
          )}
        </>
      ),
    },
    {
      key: 'tablebond',
      short: 'table bond',
      title: 'Post this table’s bond',
      done: Boolean(ours?.bond || ours?.bondAt),
      body: () => (
        <>
          {!ours?.stake ? (
            <p className="lede">This comes after the stake.</p>
          ) : (
            <Task
              label="Post this seat's bond for the table"
              note="A second payment and a different thing: this is what the seat loses for walking out mid-hand. It comes back whole if nothing is claimed."
              action="Post the bond"
              run={() => api.bondTable(table.sid)}
            />
          )}
        </>
      ),
    },
    {
      key: 'others',
      short: 'others',
      title: 'Wait for the other seats',
      done: table.funded === table.seats && table.bonded === table.seats,
      body: () => (
        <>
          <p className="lede">
            {table.funded} of {table.seats} stakes and {table.bonded} of {table.seats} bonds
            are on the chain, as this peer checked for itself. Dealing without them would
            mean playing with no answer to somebody who stops.
          </p>
          <div className="rows">
            {roster.map((s) => (
              <div className="row" key={s.seat}>
                <span>
                  seat {s.seat}
                  {s.ours && <span className="muted"> · you</span>}
                </span>
                <span className="muted">
                  {s.stake ? 'stake down' : <Awaited on={s.stakeWait} />}
                  {' · '}
                  {s.bondAt || s.bond ? 'bond down' : <Awaited on={s.bondWait} />}
                </span>
              </div>
            ))}
          </div>
          {missing.length > 0 && (
            <p className="lede warn">
              No claim can be built at this table until{' '}
              {missing.length === 1 ? `seat ${missing[0]} says` : `seats ${missing.join(', ')} say`}{' '}
              where to pay it, so it must not deal before that.
            </p>
          )}
        </>
      ),
    },
    {
      key: 'deal',
      short: 'deal',
      title: 'Play',
      done: Boolean(table.dealing),
      body: () => (
        <>
          <p className="lede">
            {table.dealing
              ? `The table is hot${table.hand ? ` and playing hand ${table.hand}` : ''}.`
              : 'The table deals as soon as every stake and every bond is on the chain.'}
          </p>
          {table.dealing && (
            <div className="row">
              <span>The cards are on the felt</span>
              <button className="act primary" onClick={onFelt}>
                Go to the felt
              </button>
            </div>
          )}
        </>
      ),
    },
  ]

  // The current step is the first unfinished one. A table that is dealing has
  // finished all of them, and stays on the last rather than falling off the end.
  const at = steps.findIndex((s) => !s.done)
  const current = at === -1 ? steps.length - 1 : at

  return (
    <section className="card">
      <ol className="rail">
        {steps.map((s, i) => (
          <li
            key={s.key}
            className={`rail-step ${i < current ? 'done' : i === current ? 'now' : 'later'}`}
            aria-current={i === current ? 'step' : undefined}
          >
            <span className="rail-pip">{i < current ? '✓' : i + 1}</span>
            <span className="rail-name">{s.short}</span>
          </li>
        ))}
      </ol>

      <h2>{steps[current].title}</h2>
      {steps[current].body(at === -1 ? 'done' : 'now')}
    </section>
  )
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="rows">
      <div className="row">
        <span>{label}</span>
        <span title={value}>{value}</span>
      </div>
    </div>
  )
}

/** Task is one thing to do, and what became of asking for it.
 *
 *  The payment is deliberately not waited on here - a person approving one may
 *  take half an hour - so this asks, gets an id, and then follows that id. The
 *  distinction it keeps visible is the one that matters: the host approving a
 *  payment and the plugin having recorded it against a seat are two facts, and
 *  the gap between them is exactly where a payment goes missing.
 *
 *  Where the approval happens is said out loud, because it is not here and a
 *  button that appeared to do nothing would be the alternative. This page runs
 *  in a sandboxed frame; it can ask for a payment and it can never take a
 *  passphrase. */
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
            setSaid(`${s.state} — approve it in your wallet, below this panel`)
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
                setSaid('asked your wallet; approve it below this panel')
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
