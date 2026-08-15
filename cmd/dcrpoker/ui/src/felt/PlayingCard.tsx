import { cardParts } from '../format'
import { Pip, suitOf } from './Pip'

// One card, honestly.
//
// Four states and only four. A face is a card this peer has actually read from
// the deck. A back is a card that exists and is not ours to read - another
// seat's hand. A gap is a card that has not been dealt, drawn as an outline
// rather than a back, because "not dealt" and "not readable by us" are
// different claims and the picture must not merge them.
//
// Opening is the fourth: dealt, and this peer is still collecting the shares
// that open it. It was drawn as a gap until a live table showed why that is
// wrong - the street had turned, the other player could see the river, and this
// one showed an empty outline that reads as a table doing nothing. The wait is
// the design working and it has to look like waiting.
//
// The enter class is how a card animates on arrival, and it is only ever put on
// a card that just appeared - which, since cards appear when they open, means
// the motion asserts nothing the protocol has not already established. The
// i1..i4 classes stagger a row. A card dealt to a seat is animated by its seat
// instead, because only the seat knows where the middle of the table is from
// where it sits.
//
// The suits are drawn, not typed. See Pip.

export function PlayingCard({
  card,
  small,
  index,
  entered,
  opening,
}: {
  /** The card as e.g. "As", "" for a back, undefined for a gap. */
  card?: string
  small?: boolean
  index?: number
  /** Whether to run the arrival animation. Left off by a caller that is doing
   *  the arrival itself. */
  entered?: boolean
  /** Shares in and shares needed, for a dealt card this peer cannot read yet.
   *  Only meaningful while card is undefined. */
  opening?: { arrived: number; needed: number }
}) {
  const size = small ? ' small' : ''
  const anim = entered ? ' enter' : ''
  const stagger = entered && index ? ` i${Math.min(index, 4)}` : ''

  if (card === undefined && opening) {
    return (
      <div className={`pcard opening${size}`} title="waiting for every seat to publish its share">
        <span className="shares">
          {opening.arrived}/{opening.needed}
        </span>
      </div>
    )
  }
  if (card === undefined) return <div className={`pcard gap${size}`} />
  if (card === '') return <div className={`pcard back${size}${anim}${stagger}`} />

  const { rank, red } = cardParts(card)
  const suit = suitOf(card)
  return (
    <div className={`pcard face${red ? ' red' : ''}${size}${anim}${stagger}`}>
      {/* Large and faint behind the rank, the way a real card carries it. It is
        * the same shape as the corner pip and takes the same colour, so it
        * reads as one card rather than as a background. */}
      <Pip suit={suit} size={small ? 34 : 46} className="pip-watermark" />
      <span className="rank">{rank}</span>
      <Pip suit={suit} size={small ? 13 : 16} className="pip-corner" />
    </div>
  )
}
