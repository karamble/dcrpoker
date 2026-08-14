import type { Duty, SeatView } from '../api'

// Who is holding the table up.
//
// The plugin has said this all along - every seat in the roster carries what it
// owes - and nothing rendered it. It is the only thing on this page with a cost
// attached to ignoring it: an obligation that stands for about six blocks is
// one the other seats may take that seat's table bond over. So it sits above
// every tab and on the felt, rather than in a card somebody has to find.
//
// There is no countdown, because there is no clock. Every deadline in this
// protocol is a block height and blocks are Poisson - a seconds bar here would
// be this machine's opinion presented as a fact.

/** What this player is being waited on for, in the second person. Absent kinds
 *  fall back to the seat's own sentence, which is the plugin's wording and is
 *  always right if never personal. */
const ours: Partial<Record<Duty['kind'], string>> = {
  cardkey: 'The table is waiting on your card key.',
  shuffle: 'It is your turn to shuffle the deck.',
  share: 'The table is waiting on your share of a card.',
  action: 'It is your turn to act.',
  checkpoint: 'The table is waiting on your signature over the last result.',
}

export function OwesAlarm({ roster, seat }: { roster: SeatView[]; seat?: number }) {
  const owing = roster.filter((s) => s.owes)
  if (owing.length === 0) return null

  const mine = seat === undefined ? undefined : owing.find((s) => s.seat === seat)
  const others = owing.filter((s) => s !== mine)

  return (
    <>
      {mine?.owes && (
        <p className="alarm" role="alert">
          {ours[mine.owes.kind] ?? mine.says ?? 'The table is waiting on you.'} Nothing is
          dealt until it arrives, and a seat that stays silent long enough can have its
          table bond taken by the others.
        </p>
      )}
      {others.map((s) => (
        <p className="lede warn" key={s.seat}>
          {s.says ?? `Seat ${s.seat} owes the table something`}. Nothing is dealt until it
          arrives.
        </p>
      ))}
    </>
  )
}
