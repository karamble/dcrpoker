import type { HandView } from '../api'

// Where this deck came from.
//
// Ranked first of everything on this screen, above the money and far above the
// felt, because it is the question a player cannot answer any other way. They
// cannot watch the cards being dealt and they cannot audit a shuffle by eye;
// what they can be told is how much of the deck's history this process checked
// for itself.
//
// Three states, and the difference between two of them is the whole point:
//
//   ours      - this peer permuted the deck itself
//   verified  - this peer checked somebody else's proof
//   awaited   - not yet
//
// "We shuffled it" is a stronger claim than "we checked their proof", and both
// are much stronger than "the network verified it" - which is a sentence this
// screen must never produce, because it is not something any single process can
// know. Every count here is a count of checks this process ran.

export function Provenance({ hand }: { hand?: HandView }) {
  const shuffles = hand?.shuffles ?? []
  if (!hand || shuffles.length === 0) {
    return (
      <section className="card">
        <h2>The deck</h2>
        <p className="lede">
          No hand is being dealt, so there is no shuffle to check yet.
        </p>
      </section>
    )
  }

  const done = shuffles.filter((s) => s.state !== 'awaited').length
  const all = done === shuffles.length

  return (
    <section className="card">
      <h2>The deck</h2>
      <p className="headline">
        {done} of {shuffles.length} shuffles verified by this process
        {hand.hand ? `, hand ${hand.hand}` : ''}.
      </p>
      <p className="lede">
        {all
          ? 'Every seat permuted the deck and this process checked each proof before building on it. No single seat knows the order.'
          : 'Each seat permutes the deck in turn. A shuffle is counted here only after this process has checked the proof that came with it.'}
      </p>
      <div className="chips">
        {shuffles.map((s) => (
          <span key={s.seat} className={`chip ${s.state}`}>
            <span className="mono">seat {s.seat}</span>
            {s.state === 'ours' && <span>you shuffled it</span>}
            {s.state === 'verified' && <span>proof checked here</span>}
            {s.state === 'awaited' && <span>waiting</span>}
          </span>
        ))}
      </div>
    </section>
  )
}
