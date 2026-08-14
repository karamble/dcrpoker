import { Lock, TriangleAlert } from 'lucide-react'
import type { Bond as BondInfo, Snapshot } from '../api'
import { dcr } from '../format'
import { BlockTick } from './BlockTick'
import { locked } from './Receipts'
import { Rolling } from './Rolling'

// What this player's coin is doing while they are not playing.
//
// Three figures and a block height. The bond is the gate - without one this
// player can join nothing - and the other two are the only honest answers to
// "how much of mine is out there": what the tables are holding, and what a
// finished table is still holding. They are different numbers because one of
// them comes back when the table pays out and the other comes back when a
// timelock does.
//
// The card below carries the bond's address, its outpoint and the way to post
// it. This is the summary; that is the account.

/** staked is what the chain is holding for this player at tables still in play:
 *  a buy-in counts once this peer has seen the stake output, not when it was
 *  announced. */
function staked(tables: Snapshot[]): { atoms: number; count: number } {
  let atoms = 0
  let count = 0
  for (const t of tables) {
    if (t.finished || t.seat === undefined) continue
    if (!(t.roster ?? []).find((s) => s.ours)?.stake) continue
    atoms += t.buyinAtoms
    count += 1
  }
  return { atoms, count }
}

export function Masthead({ tables, bond }: { tables: Snapshot[]; bond?: BondInfo }) {
  const receipts = tables.filter((t) => t.finished)
  const out = staked(tables)

  let held = 0
  let partial = false
  for (const t of receipts) {
    const a = locked(t)
    if (a === undefined) partial = true
    else held += a
  }

  let height: number | undefined
  for (const t of tables) if (t.height !== undefined) height = Math.max(height ?? 0, t.height)
  if (height === undefined) height = bond?.height

  return (
    <header className="mast">
      <div className="mast-top">
        <span className="mast-mark">poker</span>
        <span className="mast-spacer" />
        <span className="mast-height">
          {height !== undefined ? `block ${height.toLocaleString()}` : 'no chain height yet'}
        </span>
      </div>

      <div className="mast-figs">
        <BondFigure bond={bond} />

        <span className="figure">
          <b>
            <Rolling atoms={out.atoms} fromZero /> DCR
          </b>
          <small>
            staked{out.count > 0 ? ` in ${out.count} ${out.count === 1 ? 'table' : 'tables'}` : ', nothing at a table'}
          </small>
        </span>

        {receipts.length > 0 && (
          <span className="figure">
            <b className="locked">
              <Lock size={16} strokeWidth={1.75} aria-hidden />
              <Rolling atoms={held} fromZero />
              {partial ? '+' : ''} DCR
            </b>
            <small>
              locked in {receipts.length} {receipts.length === 1 ? 'receipt' : 'receipts'}
            </small>
          </span>
        )}
      </div>

      <BlockTick height={height} />
    </header>
  )
}

function BondFigure({ bond }: { bond?: BondInfo }) {
  if (!bond) {
    return (
      <span className="figure">
        <b className="muted">—</b>
        <small>asking about your bond</small>
      </span>
    )
  }

  if (!bond.hasDeposit) {
    return (
      <span className="figure none">
        <b>
          <TriangleAlert size={16} strokeWidth={1.75} aria-hidden /> no bond
        </b>
        <small>this player can join nothing</small>
      </span>
    )
  }

  if (bond.spent) {
    return (
      <span className="figure">
        <b className="muted">—</b>
        <small>nothing locked at that output</small>
      </span>
    )
  }

  const ready = bond.spendable === true
  return (
    <span className={`figure${ready ? ' free' : ''}`}>
      <b className={ready ? undefined : 'locked'}>
        {!ready && <Lock size={16} strokeWidth={1.75} aria-hidden />}
        {dcr(bond.atoms ?? bond.minAtoms)} DCR
      </b>
      <small>
        {ready
          ? 'bonded, free to take back'
          : bond.blocksLeft
            ? `bonded, free in ${bond.blocksLeft.toLocaleString()} blocks`
            : 'bonded'}
      </small>
    </span>
  )
}
