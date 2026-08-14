import type { Snapshot } from '../api'

/** Finished is a table nothing is dealt at any more, kept only because it still
 *  holds coin.
 *
 *  It does not say this player left, although that is the usual reason. A table
 *  also comes back finished if it ended before it dealt, or if it was in the
 *  middle of a hand when this process last stopped - and asserting the wrong one
 *  of those three on the screen holding somebody's money is worse than saying
 *  the thing all three have in common.
 *
 *  A record and nothing else, so it wears the record's posture: no box, one
 *  hairline. The log it used to offer is its own card now, because the audit tab
 *  wants it too and a table still being played wants it as much as this one. */
export function Finished({ table }: { table: Snapshot }) {
  return (
    <section className="card quiet">
      <h2>Finished</h2>
      <p className="headline">Nothing is dealt at this table any more.</p>
      <p className="lede">
        It is kept because it still holds coin of yours on the chain. Nobody can be seated
        at it again — the roster, the escrow and the bond all name the membership it had.
        What is below is the record.
      </p>
      {table.reason && <p className="lede warn">{table.reason}</p>}
    </section>
  )
}
