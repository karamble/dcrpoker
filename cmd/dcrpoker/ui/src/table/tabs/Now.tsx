import type { Snapshot } from '../../api'
import type { BondState } from '../../bond'
import { go } from '../../route'
import type { TableView } from '../../view'
import { Progress } from '../Progress'

/** Now answers "what do I do?".
 *
 *  One card, wearing the lead posture, and nothing beside it. The step rail is
 *  already the answer to the question and a second card here would be a second
 *  thing to read before pressing the one button that matters. Where the money is
 *  and who else is at the table are the other tabs; the owes alarm is above all
 *  of them, in the frame. */
export function Now({
  table,
  view,
  bond,
}: {
  table: Snapshot
  view: TableView
  bond: BondState
}) {
  return (
    <div className="lead-slot">
      <Progress
        table={table}
        ledger={view.ledger}
        bond={bond.bond}
        onFelt={() => go({ view: 'felt', sid: table.sid })}
      />
    </div>
  )
}
