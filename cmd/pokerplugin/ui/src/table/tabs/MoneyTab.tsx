import { Lock } from 'lucide-react'
import type { Snapshot } from '../../api'
import type { BondState } from '../../bond'
import { dcr, short } from '../../format'
import type { TableView } from '../../view'
import { Bond } from '../Bond'
import { Money } from '../Money'

/** Money answers "where is my money?" - what the table thinks the stacks are,
 *  what of yours is locked on the chain at this table, and the standing bond,
 *  which is not this table's at all. */
export function MoneyTab({
  table,
  view,
  bond,
}: {
  table: Snapshot
  view: TableView
  bond: BondState
}) {
  return (
    <>
      <Money table={table} ledger={view.ledger} />
      <Held table={table} />
      <Bond {...bond} />
    </>
  )
}

/** Held is the coin of yours that is on the chain at this table and not yours
 *  alone to move.
 *
 *  It is drawn hatched rather than coloured, because locked is a different kind
 *  of fact from good or bad. There is no countdown beside it: the lock is a
 *  number of blocks on top of the output, this page does not know which block
 *  that output landed in, and a bar counting down seconds would be this machine
 *  guessing out loud.
 *
 *  It renders nothing when there is nothing held, which is what keeps it out of
 *  the way of a table that has not been paid into yet.
 *
 *  `quiet` is for the receipt, where everything on the page is a record. Beside
 *  the money tab's other cards it is a fact rather than a record, and a
 *  borderless block between two boxed ones reads as something that failed to
 *  load. */
export function Held({ table, quiet }: { table: Snapshot; quiet?: boolean }) {
  const ours = table.roster?.find((s) => s.ours)
  const stake = ours?.stake
  const bond = ours?.bondAt || ours?.bond
  if (!stake && !bond) return null

  return (
    <section className={quiet ? 'card quiet' : 'card'}>
      <h2>Locked at this table</h2>
      {stake && (
        <>
          <div className="chips">
            <span className="fenced">
              <Lock size={16} strokeWidth={1.75} aria-hidden />
              <b>{dcr(table.buyinAtoms)} DCR</b>
              <span className="until" title={stake}>
                your stake · {short(stake, 4)}
              </span>
            </span>
          </div>
          <p className="lede muted">
            A stake moves only on a transaction every seat has signed, which is how the
            table pays out. If it never settles, it is yours alone to take back once{' '}
            {table.csvBlocks.toLocaleString()} blocks sit on top of it.
          </p>
        </>
      )}
      {bond && (
        <>
          <div className="chips">
            <span className="fenced">
              <Lock size={16} strokeWidth={1.75} aria-hidden />
              <b>this seat's bond</b>
              <span className="until" title={bond}>
                {short(bond, 4)}
                {ours?.bondAt ? ' · moved' : ''}
              </span>
            </span>
          </div>
          <p className="lede muted">
            The bond is what this seat loses for walking out mid-hand. It comes back whole
            if nothing is claimed, to you and nobody else, once its own lock matures.
          </p>
        </>
      )}
      <p className="lede muted">
        These are outputs this peer found on the chain itself, not announcements it was
        given.
      </p>
    </section>
  )
}
