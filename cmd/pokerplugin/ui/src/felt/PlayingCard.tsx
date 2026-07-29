import { cardParts } from '../format'

// One card, honestly.
//
// Three states and only three. A face is a card this peer has actually read
// from the deck. A back is a card that exists and is not ours to read - another
// seat's hand. A gap is a card that has not opened yet, drawn as an outline
// rather than a back, because "not dealt" and "not readable by us" are
// different claims and the picture must not merge them.
//
// The enter class is how a card animates on arrival, and it is only ever put on
// a card that just appeared - which, since cards appear when they open, means
// the motion asserts nothing the protocol has not already established. The i0..
// i4 classes stagger a row; fixed classes, never computed styles, so the host's
// style-src stays strict.

export function PlayingCard({
  card,
  small,
  index,
  entered,
}: {
  /** The card as e.g. "As", "" for a back, undefined for a gap. */
  card?: string
  small?: boolean
  index?: number
  entered?: boolean
}) {
  const size = small ? ' small' : ''
  const anim = entered ? ' enter' : ''
  const stagger = index ? ` i${Math.min(index, 4)}` : ''

  if (card === undefined) return <div className={`pcard gap${size}`} />
  if (card === '') return <div className={`pcard back${size}${anim}${stagger}`} />

  const { rank, suit, red } = cardParts(card)
  return (
    <div className={`pcard face${red ? ' red' : ''}${size}${anim}${stagger}`}>
      <span className="rank">{rank}</span>
      <span className="suit">{suit}</span>
    </div>
  )
}
