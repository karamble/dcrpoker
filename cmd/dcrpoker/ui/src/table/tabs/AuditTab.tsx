import type { Snapshot } from '../../api'
import type { TableView } from '../../view'
import { LogDownload } from '../LogDownload'
import { OnChain } from '../OnChain'
import { Provenance } from '../Provenance'
import { Verify } from '../Verify'

/** Audit answers "can I prove it?" - where the cards came from, what the chain
 *  has been told, the log to keep, and the two things only this client can do
 *  about any of it.
 *
 *  A receipt resumed from disk carries no hand and no play: there is no shuffle
 *  to show, usually nothing on the chain beyond the coin already named, and
 *  nothing left to challenge. Those cards are left out rather than rendered
 *  empty. A card that says nothing is worse than no card - it reads as an answer
 *  that has not arrived. */
export function AuditTab({ table, view }: { table: Snapshot; view: TableView }) {
  const ledger = view.ledger
  const dealt = (view.hand?.shuffles?.length ?? 0) > 0
  const onChain =
    (ledger?.events?.length ?? 0) > 0 ||
    (ledger?.claims?.length ?? 0) > 0 ||
    (ledger?.challenges?.length ?? 0) > 0 ||
    (ledger?.disputes?.length ?? 0) > 0 ||
    Boolean(ledger?.settlement)

  return (
    <>
      {(dealt || !table.finished) && <Provenance hand={view.hand} />}
      {(onChain || !table.finished) && (
        <OnChain
          ledger={ledger}
          dealing={table.dealing}
          accused={ledger?.accused}
          over={table.over}
        />
      )}
      <LogDownload sid={table.sid} />
      {!table.finished && (
        <Verify variant="card" table={table} ledger={ledger} hand={view.hand} names={view.names} />
      )}
    </>
  )
}
