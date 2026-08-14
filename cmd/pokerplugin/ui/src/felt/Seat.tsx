import { motion } from 'framer-motion'
import type { Chair, SeatView } from '../api'
import { dcr } from '../format'
import { dealt, travel } from './motion'
import { PlayingCard } from './PlayingCard'
import type { Spot } from './positions'

// One chair, saying everything a poker screen owes about it.
//
// The stack, what they have put in this street, the last thing they did, their
// cards to the extent the protocol has opened them, the dealer button, and the
// three states a seat can be in - to act, folded, all in. Most of this simply
// was not shown before, which made the felt a diagram rather than a game.
//
// What the cards may show is decided elsewhere, on purpose. Our own come from
// hole, an opponent's from shown - which only ever holds hands a showdown
// actually opened, because a folded seat never published the shares that would
// open its slots. This component draws whatever it is given and nothing more,
// and it can afford that only because the deciding happened where it belongs.
//
// Two arrivals, and they mean different things. A card dealt flies in from the
// middle of the table, once per hand, because that is where it came from. A
// card that turns over in place is one that was already here and has opened -
// a showdown - and it keeps the flip it always had.

const said: Record<string, string> = {
  check: 'checked',
  call: 'called',
  bet: 'bet',
  raise: 'raised to',
  allin: 'all in',
  fold: 'folded',
}

export function Seat({
  pos,
  spot,
  deal,
  hand,
  chair,
  view,
  name,
  ours,
  turn,
  dealer,
  won,
  cards,
  fallbackStack,
}: {
  /** Display position 0..n-1, ours rotated to 0 (the bottom). */
  pos: number
  /** Where this position sits on the table, as percentages. */
  spot: Spot
  /** The offset in pixels from this seat to the middle of the table, which is
   *  where a card is dealt from. Zero before the table has been measured. */
  deal: { x: number; y: number }
  /** The hand these cards belong to. Cards arrive once per hand, so this is
   *  what makes the deal run again rather than every render. */
  hand?: number
  chair?: Chair
  view?: SeatView
  name?: string
  ours: boolean
  turn: boolean
  dealer: boolean
  won: boolean
  /** Two entries; "" is a back, undefined a gap. */
  cards: (string | undefined)[]
  /** What to show between hands, when there is no chair. */
  fallbackStack?: number
}) {
  const folded = chair?.folded
  const allin = chair?.allIn
  const stack = chair ? chair.stack : fallbackStack

  const move = () => {
    if (folded) return <span className="did">folded</span>
    if (allin) return <span className="did">all in</span>
    if (!chair?.last || chair.last === 'fold') return null
    const word = said[chair.last] ?? chair.last
    const withAmount = chair.last === 'bet' || chair.last === 'raise'
    return (
      <span className="did">
        {word}
        {withAmount && chair.lastAmount ? ` ${dcr(chair.lastAmount)}` : ''}
      </span>
    )
  }

  return (
    <div
      className={`seat${turn ? ' turn' : ''}${folded ? ' folded' : ''}${allin ? ' allin' : ''}${
        won ? ' win' : ''
      }`}
      style={{ left: `${spot.left}%`, top: `${spot.top}%` }}
    >
      <div className="seat-cards">
        {[0, 1].map((i) => {
          // The key carries whether there is a card at all, not just which
          // hand it is. A slot goes from gap to card partway through a hand -
          // the shuffle finishes and the deal opens - and keying on the hand
          // alone would leave our own cards to appear without ever arriving.
          const dealtYet = cards[i] !== undefined
          return (
            <motion.div
              className="dealt"
              key={`${hand ?? 0}-${i}-${dealtYet ? 'card' : 'gap'}`}
              variants={dealt(deal.x, deal.y, i)}
              // A gap is a card that has not been dealt. It does not arrive
              // from anywhere, because it has not arrived.
              initial={dealtYet ? 'from' : false}
              animate="at"
            >
              <PlayingCard
                card={cards[i]}
                small
                // A face at a seat that is not ours is a hand a showdown has
                // opened. It turns over where it lies; it is not dealt again.
                entered={!ours && Boolean(cards[i])}
              />
            </motion.div>
          )
        })}
      </div>
      <div className="seat-plate">
        {dealer && (
          <motion.span layoutId="dealer" className="dealer" transition={travel}>
            D
          </motion.span>
        )}
        <div className="seat-name" title={view?.key}>
          <span>{name || `seat ${view?.seat ?? pos}`}</span>
          {ours && <span className="you">you</span>}
        </div>
        <div className="seat-stack">{stack !== undefined ? dcr(stack) : '—'}</div>
        <div className="seat-move">{move()}</div>
      </div>
    </div>
  )
}
