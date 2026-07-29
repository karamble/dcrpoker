import type { Chair, SeatView } from '../api'
import { dcr } from '../format'
import { PlayingCard } from './PlayingCard'

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
      className={`seat pos${pos}${turn ? ' turn' : ''}${folded ? ' folded' : ''}${
        allin ? ' allin' : ''
      }${won ? ' win' : ''}`}
    >
      <div className="seat-cards">
        {[0, 1].map((i) => (
          <PlayingCard key={i} card={cards[i]} small entered={cards[i] !== undefined} />
        ))}
      </div>
      <div className="seat-plate">
        {dealer && <span className="dealer">D</span>}
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
