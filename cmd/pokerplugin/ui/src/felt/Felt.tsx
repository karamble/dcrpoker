import type { Chair, HandView, SeatView } from '../api'
import { dcr } from '../format'
import { Card } from './Card'

// The felt.
//
// Ranked last of the four things on this screen, deliberately, and drawn in SVG
// rather than in anything three-dimensional. The hard problem in a trustless
// poker client is not the table; it is making the trust model legible, and a
// prettier felt improves none of that. It is also the only part that would have
// to bring textures, models and fonts into a bundle that has no route out to
// fetch any of them.
//
// Our own seat is rotated to the bottom. That is the one piece of arrangement
// that matters: a player reads their own position first and everything else
// relative to it.

const VIEW_W = 520
const VIEW_H = 400
const RING_X = 260
const RING_Y = 170
const RX = 160
const RY = 88

// How far out the seats sit from the felt's edge. Far enough that a seat's
// cards fit between its box and the oval without landing on either.
const SEAT_OUT_X = 62
const SEAT_OUT_Y = 78

const BOX_W = 112
const BOX_H = 46
const HOLE_W = 26
const HOLE_H = HOLE_W * 1.4

export function Felt({ hand, roster }: { hand: HandView; roster: SeatView[] }) {
  const seats = hand.stacks.length
  const chairs = hand.chairs ?? []
  const street = chairs.reduce((sum, c) => sum + c.committed, 0)
  const board = hand.board ?? []
  const boardSlots = Array.from({ length: 5 }, (_, i) => ({
    card: board[i],
    // A board card that has not opened yet is not hidden from us
    // specifically - nobody has it. So it stays a waiting slot.
    hidden: false,
  }))

  return (
    <svg
      className="felt"
      viewBox={`0 0 ${VIEW_W} ${VIEW_H}`}
      role="img"
      aria-label="the table"
    >
      <ellipse className="felt-oval" cx={RING_X} cy={RING_Y} rx={RX} ry={RY} />

      <text className="felt-pot-label" x={RING_X} y={118} textAnchor="middle">
        POT
      </text>
      <text className="felt-pot" x={RING_X} y={136} textAnchor="middle">
        {dcr(hand.pot)}
      </text>
      {street > 0 && (
        // Chips committed on this street are not in the pot yet - an uncalled
        // bet has to be returnable - so they are counted beside it, not added
        // to it.
        <text className="felt-label" x={RING_X} y={150} textAnchor="middle">
          +{dcr(street)} on this street
        </text>
      )}

      {boardSlots.map((slot, i) => (
        <Card key={i} x={RING_X - 93 + i * 38} y={160} w={34} slot={slot} />
      ))}

      <text className="felt-label" x={RING_X} y={228} textAnchor="middle">
        {hand.street || '—'}
      </text>

      {Array.from({ length: seats }, (_, seat) => (
        <Seat
          key={seat}
          seat={seat}
          of={seats}
          ours={seat === hand.seat}
          mineIndex={hand.seat}
          hand={hand}
          view={roster.find((s) => s.seat === seat)}
          chair={chairs.find((c) => c.seat === seat)}
        />
      ))}
    </svg>
  )
}

function Seat({
  seat,
  of,
  ours,
  mineIndex,
  hand,
  view,
  chair,
}: {
  seat: number
  of: number
  ours: boolean
  mineIndex: number
  hand: HandView
  view?: SeatView
  chair?: Chair
}) {
  // Rotate so this player sits at the bottom of the oval, then walk the rest
  // round from there in seat order.
  const step = ((seat - mineIndex + of) % of) / of
  const angle = Math.PI / 2 + step * 2 * Math.PI
  const cx = RING_X + Math.cos(angle) * (RX + SEAT_OUT_X)
  const cy = RING_Y + Math.sin(angle) * (RY + SEAT_OUT_Y)

  const x = clamp(cx - BOX_W / 2, 2, VIEW_W - BOX_W - 2)
  const y = clamp(cy - BOX_H / 2, 2, VIEW_H - BOX_H - 2)

  // Cards go on the side of the box that faces the felt, so they never land on
  // the box's own text and never leave the picture.
  const below = cy < RING_Y
  const cardY = below ? y + BOX_H + 4 : y - HOLE_H - 4
  const cardX = x + BOX_W / 2 - HOLE_W - 3

  const turn = hand.toAct === seat
  const hole = ours ? (hand.hole ?? []) : []
  // What is behind them now, not what they signed for at the last boundary.
  const stack = chair ? chair.stack : hand.stacks[seat]
  const out = chair?.folded || chair?.allIn

  return (
    <g>
      {[0, 1].map((i) => (
        <Card
          key={i}
          x={cardX + i * (HOLE_W + 4)}
          y={cardY}
          w={HOLE_W}
          slot={ours ? { card: hole[i] } : { hidden: true }}
        />
      ))}

      <rect
        className={`seat-box${turn ? ' turn' : ''}${ours ? ' ours' : ''}`}
        x={x}
        y={y}
        width={BOX_W}
        height={BOX_H}
        rx={6}
      />
      <text className="seat-name" x={x + 8} y={y + 16}>
        seat {seat}
        {ours ? ' · you' : ''}
      </text>
      <text className="seat-stack" x={x + 8} y={y + 29}>
        {dcr(stack)}
      </text>
      {chair !== undefined && chair.committed > 0 && (
        // In front of them, not in the pot. It goes in when the street ends.
        <text className="seat-bet" x={x + BOX_W - 8} y={y + 29} textAnchor="end">
          {dcr(chair.committed)}
        </text>
      )}
      {out && (
        <text className="seat-folded" x={x + 8} y={y + 40}>
          {chair?.folded ? 'folded' : 'all in'}
        </text>
      )}
      {!out && view?.leaving && (
        <text className="seat-folded" x={x + 8} y={y + 40}>
          leaving
        </text>
      )}
      {!out && !view?.leaving && !ours && view?.says && (
        // The honest replacement for a "disconnected" light. The protocol has
        // no notion of somebody being away - only of an obligation that is not
        // met - so that is what it says.
        //
        // Not on our own seat: what this player owes is on the buttons in
        // front of them, and saying it twice reads as an accusation rather
        // than as a prompt.
        <text className="seat-owes" x={x + 8} y={y + 40}>
          {shorten(view.says)}
        </text>
      )}
    </g>
  )
}

function clamp(v: number, lo: number, hi: number) {
  return Math.min(Math.max(v, lo), hi)
}

/** shorten trims a duty sentence to what fits on a seat box without becoming a
 *  different sentence. The full one is on the table view, where there is room
 *  for it. */
function shorten(says: string): string {
  const trimmed = says.replace(/^seat \d+ owes /, 'owes ')
  return trimmed.length > 21 ? `${trimmed.slice(0, 20)}…` : trimmed
}
