import type { ReactNode } from 'react'
import type { SeatView } from '../api'
import { ErrorBanner } from './ErrorBanner'
import { LiveDot } from './LiveDot'
import { OwesAlarm } from './OwesAlarm'

// The frame the lobby and the table view sit in.
//
// Four things in a fixed order: where you are and how to get back, whether the
// page is being told or asking, what went wrong, and who is holding the table
// up. They are here rather than in each route because their order is the point
// - a player must not have to scroll past the tabs to find out that the table
// is waiting on them.
//
// The felt does not use this. It has its own bar, and the whole argument for
// the felt is that nothing on it moves.

export function Chrome({
  live,
  error,
  roster,
  seat,
  lead,
  children,
}: {
  live: boolean
  error?: string
  roster?: SeatView[]
  seat?: number
  /** What sits at the head of the bar. The lobby has nowhere to go back to. */
  lead?: ReactNode
  children: ReactNode
}) {
  return (
    <div className="app">
      <nav className="bar">
        {lead}
        <span className="bar-spacer" />
        <LiveDot live={live} />
      </nav>
      <main className="body">
        <ErrorBanner error={error} />
        <OwesAlarm roster={roster ?? []} seat={seat} />
        {children}
      </main>
    </div>
  )
}
