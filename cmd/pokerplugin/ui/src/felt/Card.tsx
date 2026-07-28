import { cardParts } from '../format'

// One card, or the absence of one, and the difference between two absences.
//
// A slot this peer cannot read is drawn differently from a slot with no card in
// it. The plugin is careful about this - it omits a card it cannot open rather
// than sending a blank - and the reason survives all the way to here: "not
// dealt" is something to wait through and "dealt but not readable by you" is
// either an opponent's hand or something stuck. Rendering both as an empty
// space would throw away the only signal that tells them apart.

export type Slot = { card?: string; hidden?: boolean }

export function Card({
  x,
  y,
  w = 30,
  slot,
}: {
  x: number
  y: number
  w?: number
  slot: Slot
}) {
  const h = w * 1.4
  const r = 3

  if (slot.card) {
    const { rank, suit, red } = cardParts(slot.card)
    return (
      <g>
        <rect className="pc-face" x={x} y={y} width={w} height={h} rx={r} />
        <text className={`pc-rank${red ? ' red' : ''}`} x={x + w / 2} y={y + h * 0.44} textAnchor="middle">
          {rank}
        </text>
        <text
          className={`pc-suit${red ? ' red' : ''}`}
          x={x + w / 2}
          y={y + h * 0.8}
          textAnchor="middle"
          fill={red ? '#bd3f38' : '#1c1c21'}
        >
          {suit}
        </text>
      </g>
    )
  }

  if (slot.hidden) {
    // Face down: there is a card there and it is somebody else's.
    return <rect className="pc-back" x={x} y={y} width={w} height={h} rx={r} />
  }

  // Nothing dealt into this slot yet. Dashed, so waiting never reads as a card
  // that failed to arrive.
  return (
    <g>
      <rect className="pc-hidden" x={x} y={y} width={w} height={h} rx={r} />
      <text className="pc-mark" x={x + w / 2} y={y + h * 0.65} textAnchor="middle">
        ·
      </text>
    </g>
  )
}
