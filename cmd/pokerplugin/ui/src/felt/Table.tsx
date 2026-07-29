import { useEffect, useRef, useState } from 'react'
import type { HandView, SeatView } from '../api'
import { dcr } from '../format'
import { PlayingCard } from './PlayingCard'
import { Seat } from './Seat'

// The table, filling the space it is given.
//
// Every seat is drawn from the roster and rotated so this player sits at the
// bottom, which is where every poker player on earth expects to be. The
// rotation is display only - seat numbers, the log and the money all keep the
// protocol's numbering, and the plate says which seat a chair is when a name is
// missing.
//
// Between hands there is no HandView, and the table still stands: the seats,
// the stacks every member signed for, and no cards. A table that vanished
// between hands would read as a table that crashed between hands.

// rotate maps a protocol seat to a display position, ours at 0.
function rotate(seat: number, ours: number, n: number): number {
  return (seat - ours + n) % n
}

export function Table({
  hand,
  roster,
  ourSeat,
  stacks,
  won,
}: {
  hand?: HandView
  roster: SeatView[]
  /** Our protocol seat, for the rotation. */
  ourSeat?: number
  /** Fallback stacks between hands, when there is no HandView. */
  stacks?: number[]
  /** Seats being celebrated, at the end of a held showdown. */
  won?: number[]
}) {
  const n = Math.max(roster.length, 2)
  const ours = ourSeat ?? roster.find((s) => s.ours)?.seat ?? 0

  // The pot bumps when it grows. Growth is read from the signed entries the
  // plugin already folded into pot/committed, so the motion trails the log
  // rather than the button press.
  const pot = hand?.pot ?? 0
  const prevPot = useRef(pot)
  const [grew, setGrew] = useState(false)
  useEffect(() => {
    if (pot > prevPot.current) {
      setGrew(true)
      const id = window.setTimeout(() => setGrew(false), 350)
      return () => window.clearTimeout(id)
    }
    prevPot.current = pot
  }, [pot])
  useEffect(() => {
    prevPot.current = pot
  })

  const board = hand?.board ?? []
  const shownBy = new Map((hand?.shown ?? []).map((s) => [s.seat, s.cards]))

  const cardsFor = (seat: number, isOurs: boolean): (string | undefined)[] => {
    if (!hand || hand.phase === 'shuffling') return [undefined, undefined]
    if (isOurs) {
      const hole = hand.hole ?? []
      return [hole[0], hole[1]]
    }
    const opened = shownBy.get(seat)
    if (opened) return [opened[0], opened[1]]
    // A back is a claim that the card exists and is not ours to read, which
    // is only true once dealing has begun.
    return ['', '']
  }

  return (
    <div className={`table-wrap s${n}`}>
      <div className="oval" />

      <div className="center">
        <div className={`pot${grew ? ' grew' : ''}`}>
          pot <b>{dcr(pot)}</b>
        </div>
        <div className="board">
          {[0, 1, 2, 3, 4].map((i) => (
            <PlayingCard key={i} card={board[i]} index={i} entered={board[i] !== undefined} />
          ))}
        </div>
      </div>

      {roster.map((s) => {
        const pos = rotate(s.seat, ours, n)
        const chair = hand?.chairs?.find((c) => c.seat === s.seat)
        const committed = chair?.committed ?? 0
        return (
          <div key={s.seat} className="contents">
            <Seat
              pos={pos}
              chair={chair}
              view={s}
              name={s.name}
              ours={s.seat === ours}
              turn={Boolean(hand && hand.phase === 'betting' && hand.toAct === s.seat)}
              dealer={hand?.button === s.seat}
              won={Boolean(won?.includes(s.seat))}
              cards={cardsFor(s.seat, s.seat === ours)}
              fallbackStack={stacks?.[s.seat]}
            />
            {committed > 0 && (
              <div className={`bet bpos${pos} enter`} key={`${hand?.hand}-${hand?.street}-${committed}`}>
                <span className="disc" />
                {dcr(committed)}
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}
