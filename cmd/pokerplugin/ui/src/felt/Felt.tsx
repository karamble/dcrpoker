import type { Snapshot } from '../api'
import { go } from '../route'
import type { TableView } from '../view'
import { LiveDot } from '../shell/LiveDot'
import { OwesAlarm } from '../shell/OwesAlarm'
import { ActionBar } from './ActionBar'
import { Status } from './Status'
import { Table } from './Table'
import { TableOver } from './TableOver'
import { Verify } from '../table/Verify'

// The felt, where the cards are.
//
// One fixed layout and nothing in it moves: the verify rail at the elbow, the
// table filling the middle, one status line, one action bar. The money cards
// and the record are a click away rather than below, because a player at a live
// table should not be able to scroll the cards off the screen.
//
// It does not use Chrome. The bar here is shorter on purpose and the owes alarm
// is placed against the status line, where a player's eyes already are.

export function Felt({
  table,
  view,
  live,
}: {
  table: Snapshot
  view: TableView
  live: boolean
}) {
  const { ledger, hand, liveHand, holding, roster, names } = view

  // Who to light up as having won. Awards are compared against what the seat
  // put in, so a seat that got its own uncalled bet back is not drawn as a
  // winner. Absent awards mean this peer does not know yet, which is not zero.
  const winners =
    holding && (holding.view.awards?.length ?? 0) > 0
      ? (holding.view.awards ?? [])
          .filter((a) => {
            const paid = holding.view.chairs?.find((c) => c.seat === a.seat)?.total ?? 0
            return a.atoms > paid
          })
          .map((a) => a.seat)
      : undefined

  const details = (tab: 'now' | 'audit') => () =>
    go({ view: 'table', sid: table.sid, tab })

  return (
    <div className="stage">
      <Verify table={table} ledger={ledger} hand={hand} names={names} />
      <div className="arena">
        <header className="topbar">
          <span className="hand-no">hand {hand?.hand ?? table.hand ?? '—'}</span>
          {hand?.street && hand.phase === 'betting' && <span>{hand.street}</span>}
          <span className="spacer" />
          <LiveDot live={live} terse />
          <button className="link" onClick={details('now')}>
            Details
          </button>
        </header>
        <div className="table-zone">
          <Table
            hand={hand}
            roster={roster}
            ourSeat={table.seat}
            stacks={table.live ?? table.settled?.stacks}
            won={winners}
          />
          {/* The end of the table, after the last hand has had its fifteen
            * seconds. It covers the felt rather than the whole stage, so the
            * verify rail keeps saying what the payout below is made of. */}
          {table.over && !holding && (
            <TableOver
              table={table}
              ledger={ledger}
              onDetails={details('audit')}
              onClose={() => go({ view: 'lobby' })}
            />
          )}
        </div>
        {/* A row of the arena's grid, so the status line and the buttons below
          * keep their places whether or not anybody is being waited on. It
          * collapses when the alarm renders nothing. */}
        <div className="felt-alarm">
          <OwesAlarm roster={roster} seat={table.seat} />
        </div>
        <Status table={table} hand={hand} holdUntil={holding?.until} names={names} />
        <ActionBar hand={holding ? undefined : liveHand} />
      </div>
    </div>
  )
}
