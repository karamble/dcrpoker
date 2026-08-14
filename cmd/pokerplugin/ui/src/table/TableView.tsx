import { ArrowRight } from 'lucide-react'
import type { Snapshot } from '../api'
import type { BondState } from '../bond'
import { dcr, short } from '../format'
import { go, type Route, type Tab } from '../route'
import { standing } from '../standing'
import type { TableView as Derived } from '../view'
import { Finished } from './Finished'
import { panelId, tabId, Tabs } from './Tabs'
import { AuditTab } from './tabs/AuditTab'
import { Held, MoneyTab } from './tabs/MoneyTab'
import { Now } from './tabs/Now'
import { RosterTab } from './tabs/RosterTab'

// One table, four questions.
//
// Six cards of equal weight in one scroll left somebody to work out which one
// was waiting on them. Each tab is one question instead - what do I do, where
// is my money, who is here, can I prove it - and the tab is in the address bar,
// so a reload comes back to the same one.
//
// The header is above the strip rather than inside a tab, because which table
// this is and whether it is dealing are true on all four.

type TableRoute = Extract<Route, { view: 'table' }>

const order: readonly Tab[] = ['now', 'money', 'roster', 'audit']

export function TableView({
  route,
  view,
  bond,
}: {
  route: TableRoute
  view: Derived
  bond: BondState
}) {
  const table = view.table
  if (!table) return null

  // A finished table is a receipt: nothing to do at it, nobody to leave, and no
  // progress to make. What is left is the record and the coin still on the
  // chain, and that is one page rather than four tabs, three of which would be
  // asking questions this table no longer has answers to.
  if (table.finished) {
    return (
      <>
        <Head table={table} view={view} />
        <Finished table={table} />
        <Held table={table} quiet />
        <AuditTab table={table} view={view} />
      </>
    )
  }

  return (
    <>
      <Head table={table} view={view} />
      <Tabs sid={route.sid} tab={route.tab} tabs={order} />
      <div className="sheet" id={panelId} role="tabpanel" aria-labelledby={tabId(route.tab)}>
        <Body tab={route.tab} table={table} view={view} bond={bond} />
      </div>
    </>
  )
}

function Body({
  tab,
  table,
  view,
  bond,
}: {
  tab: Tab
  table: Snapshot
  view: Derived
  bond: BondState
}) {
  switch (tab) {
    case 'money':
      return <MoneyTab table={table} view={view} bond={bond} />
    case 'roster':
      return <RosterTab table={table} view={view} />
    case 'audit':
      return <AuditTab table={table} view={view} />
    default:
      return <Now table={table} view={view} bond={bond} />
  }
}

/** Head is the table itself, drawn in the table's own colours so that a tab is
 *  plainly part of it rather than a page about it.
 *
 *  What it says comes from `standing`, the same derivation the lobby row does,
 *  so that pressing a row saying one thing does not open a page saying another.
 *  Nothing here reads `state` for itself - `settled` alone is two situations
 *  with nothing in common from where a player sits, and that is exactly the
 *  mistake that module exists to make impossible. */
function Head({ table, view }: { table: Snapshot; view: Derived }) {
  const said = standing(table, view.hand)
  return (
    <header className="table-head">
      <Chairs seats={table.seats} taken={table.joined} ours={view.seat} />
      <span className="head-what">
        <b className="head-title">{said.say}</b>
        <span className="head-note">
          {said.note ? `${said.note} · ` : ''}
          <span className="sid" title={table.sid}>
            {short(table.sid, 4)}
          </span>
        </span>
      </span>
      <span className="head-spacer" />
      {/* .figure is the lobby's generic number-with-a-label, used as it is found
          so a buy-in reads the same here as it does in the row it was opened
          from. */}
      <span className="figure end">
        <b>{dcr(table.buyinAtoms)} DCR</b>
        <small>buy-in</small>
      </span>
      {table.dealing && (
        // A table that is over keeps the way back, because the felt is where the
        // end of it is drawn. It is not the loud button any more.
        <button
          className={table.over ? 'act' : 'act primary'}
          onClick={() => go({ view: 'felt', sid: table.sid })}
        >
          {table.over ? 'See how it ended' : 'Go to the felt'}
          <ArrowRight size={16} strokeWidth={1.75} aria-hidden />
        </button>
      )}
    </header>
  )
}

/** Chairs is the count as a picture. Seats are not identities until the draw,
 *  so before it these are how many have answered and not who sits where - which
 *  is what the label says. */
function Chairs({ seats, taken, ours }: { seats: number; taken: number; ours?: number }) {
  const label =
    ours === undefined
      ? `${taken} of ${seats} seats taken`
      : `${taken} of ${seats} seats taken, and you are seat ${ours}`
  return (
    <span className="rail-seats" role="img" aria-label={label}>
      {Array.from({ length: seats }, (_, i) => (
        <span key={i} className={`ch${ours === i ? ' ours' : i < taken ? ' taken' : ''}`} />
      ))}
    </span>
  )
}
