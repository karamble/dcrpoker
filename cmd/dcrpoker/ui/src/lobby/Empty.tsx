import type { Bond as BondInfo } from '../api'

// No table, said honestly.
//
// This game cannot make a table or ask to join one - a join arrives over the
// bridge when somebody accepts a gaming invitation in the console - so there is
// no Create button to draw and no point pretending there could be. What an
// empty lobby owes instead is where a table comes from, and whether this player
// is in a position to take a seat when one is offered.
//
// The oval is the felt's own geometry at rest. It is the only decoration here,
// it costs nothing, and it is what keeps an empty screen looking like a room
// that is waiting rather than a page that failed.

export function Empty({ loaded, bond }: { loaded: boolean; bond?: BondInfo }) {
  if (!loaded) {
    return (
      <section className="lempty">
        <div className="lempty-plate">
          <div className="lempty-oval" aria-hidden />
          <h2>Looking</h2>
          <p>Asking the plugin which tables this player is at.</p>
        </div>
      </section>
    )
  }

  return (
    <section className="lempty">
      <div className="lempty-plate">
        <div className="lempty-oval" aria-hidden />
        <h2>No table is dealt to this player</h2>
        <p>
          This game cannot make a table or ask to join one. A table arrives when somebody
          sends a gaming invitation in a group chat and it is accepted in the dashboard, in
          Bison Relay &gt; Gaming. Then it appears here.
        </p>
        {bond && !bond.hasDeposit ? (
          <p className="bad">
            No bond is posted, so no seat can be taken either. The card below is where that
            is done.
          </p>
        ) : bond ? (
          <p className="muted">
            Your bond is posted, so a seat can be taken as soon as one is offered.
          </p>
        ) : null}
      </div>
    </section>
  )
}
