import { AnimatePresence, motion, useReducedMotion } from 'framer-motion'
import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { HandView, SeatView } from '../api'
import { dcr } from '../format'
import { useTableSound } from '../sound/useTableSound'
import { sweep } from './motion'
import { PlayingCard } from './PlayingCard'
import { betAt, centre, seatAt, spotFor } from './positions'
import { Seat } from './Seat'
import { SoundToggle } from './SoundToggle'

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
//
// The table measures itself because two things need to know how big it is in
// pixels: a card dealt from the middle has to cover the distance to its seat,
// and chips have to reach the pot from wherever the seat happens to be. Both
// used to be impossible - the positions were fixed stylesheet rules, so the
// only chip sweep that existed was two hand-written keyframes for a heads-up
// table, and every larger table fell back to a fade.

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
  const still = useReducedMotion()

  useTableSound(hand, roster, ourSeat)

  // How big the felt is, in pixels. Zero until it has been laid out, which the
  // motion treats as "no distance to cover" and so animates in place.
  const wrap = useRef<HTMLDivElement>(null)
  const [size, setSize] = useState({ w: 0, h: 0 })
  useLayoutEffect(() => {
    const el = wrap.current
    if (!el) return
    const read = () => setSize({ w: el.clientWidth, h: el.clientHeight })
    read()
    const ro = new ResizeObserver(read)
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

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
  // A dealt card this peer has not opened yet. Keyed by place on the board, so
  // a slot the plugin did not report simply has none and draws as a gap.
  const openingAt = new Map(
    (hand?.opening ?? []).filter((o) => !o.open).map((o) => [o.index, o]),
  )
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
    <div className="table-wrap" ref={wrap}>
      <div className="oval" />
      <SoundToggle />

      <div className="center">
        <div className={`pot${grew ? ' grew' : ''}`}>
          pot <b>{dcr(pot)}</b>
        </div>
        <div className="board">
          {[0, 1, 2, 3, 4].map((i) => (
            <PlayingCard
              key={i}
              card={board[i]}
              index={i}
              entered={board[i] !== undefined}
              opening={openingAt.get(i)}
            />
          ))}
        </div>
      </div>

      {roster.map((s) => {
        const pos = rotate(s.seat, ours, n)
        const seat = spotFor(seatAt, n, pos)
        const bet = spotFor(betAt, n, pos)
        const chair = hand?.chairs?.find((c) => c.seat === s.seat)
        const committed = chair?.committed ?? 0
        return (
          <div key={s.seat} className="contents">
            <Seat
              pos={pos}
              spot={seat}
              deal={{
                x: ((centre.left - seat.left) / 100) * size.w,
                y: ((centre.top - seat.top) / 100) * size.h,
              }}
              hand={hand?.hand}
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
            {/* Chips in front of a seat, and where they go when the street
              * ends. The sweep is the one thing here that needs the chips to
              * outlive the state that put them there: React unmounts them the
              * moment the street closes, and an unmount has no CSS. */}
            <AnimatePresence>
              {committed > 0 && (
                <motion.div
                  className="bet"
                  key={s.seat}
                  initial={{ left: `${bet.left}%`, top: `${bet.top}%`, x: '-50%', y: '-50%', opacity: 0, scale: 0.6 }}
                  animate={{ left: `${bet.left}%`, top: `${bet.top}%`, x: '-50%', y: '-50%', opacity: 1, scale: 1 }}
                  exit={
                    still
                      ? { opacity: 0 }
                      : { left: `${centre.left}%`, top: `${centre.top + 4}%`, opacity: 0 }
                  }
                  transition={sweep}
                >
                  <span className="disc" />
                  <motion.span key={committed} initial={{ scale: 0.8 }} animate={{ scale: 1 }}>
                    {dcr(committed)}
                  </motion.span>
                </motion.div>
              )}
            </AnimatePresence>
          </div>
        )
      })}
    </div>
  )
}
