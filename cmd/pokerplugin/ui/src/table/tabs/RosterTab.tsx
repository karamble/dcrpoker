import type { Snapshot } from '../../api'
import type { TableView } from '../../view'
import { Lifecycle } from '../Lifecycle'

/** Roster answers "who is here, on what terms?" - the seats first, then the
 *  terms every one of them is bound to. Leave lives inside Lifecycle, at the
 *  bottom of the terms it ends. */
export function RosterTab({ table, view }: { table: Snapshot; view: TableView }) {
  return (
    <>
      <Who table={table} view={view} />
      <Lifecycle table={table} />
    </>
  )
}

/** Who is the seats, in seat order.
 *
 *  A name is what the host calls the identity that spoke for a seat and nothing
 *  stronger. Nothing here decides anything by it, and a seat with no name is a
 *  seat number - which is the only thing the protocol itself agrees on. */
function Who({ table, view }: { table: Snapshot; view: TableView }) {
  const roster = view.roster
  if (roster.length === 0) {
    return (
      <section className="card">
        <h2>The seats</h2>
        <p className="lede">
          Nobody has a seat yet. {table.joined} of {table.seats} players have joined, and the
          order they sit in is drawn from a block once the last one does.
        </p>
      </section>
    )
  }

  return (
    <section className="card">
      <h2>
        The seats
        <span className="aside">
          {roster.length} of {table.seats}
        </span>
      </h2>
      <table className="grid">
        <thead>
          <tr>
            <th>Seat</th>
            <th>Who</th>
            <th>Standing</th>
          </tr>
        </thead>
        <tbody>
          {roster.map((s) => (
            <tr key={s.seat} className={s.ours ? 'ours' : undefined}>
              <td>
                seat {s.seat}
                {s.ours && <span className="muted"> · you</span>}
              </td>
              <td>{view.names.get(s.seat) ?? <span className="muted">unnamed</span>}</td>
              <td>
                {s.leaving ? (
                  'getting up'
                ) : s.owes ? (
                  <span className="warn">{s.says ?? 'owes the table something'}</span>
                ) : (
                  <span className="muted">nothing owed</span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="lede muted">
        A name is what the host calls the identity that spoke for a seat. Nothing here
        decides anything by it, and the seat number is what the protocol agrees on.
      </p>
    </section>
  )
}
