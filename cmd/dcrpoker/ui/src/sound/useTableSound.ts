import { useEffect, useRef } from 'react'
import type { HandView, SeatView } from '../api'
import { play } from './engine'

// Saying out loud what the felt has just drawn.
//
// Everything below is read from a HandView the plugin already sent, so a sound
// trails the signed log exactly the way the motion does. Nothing is heard
// before the thing it is about has happened, and nothing is heard that the
// screen is not also showing.
//
// One hook, called once, rather than a play() scattered through the components.
// The felt's components are pure and worth keeping that way, and one place to
// read is one place to audit.
//
// Everything is keyed on an identity rather than on "changed since the last
// render". StrictMode runs effects twice in development, and a diff would deal
// every hand twice.

/** primed marks the first view of a hand as scenery rather than news.
 *
 *  A page opened into a hand already in progress would otherwise deal its cards
 *  aloud, which would be a sound saying something that happened ten minutes
 *  ago. */
type Seen = {
  primed: boolean
  /** The hand everything below is counted within. A new one clears the rest,
   *  whatever phase it is in - a chair carried over from the last hand would
   *  otherwise be diffed against this one and heard as somebody acting. */
  hand: number
  dealt: number
  board: number
  moves: string
  won: number
}

export function useTableSound(hand: HandView | undefined, roster: SeatView[], ourSeat?: number): void {
  const seen = useRef<Seen>({ primed: false, hand: -1, dealt: -1, board: 0, moves: '', won: -1 })

  // What the table is waiting on this seat for. Present the moment it is our
  // turn, so presence alone is not news - see the owes effect below.
  const mine = ourSeat === undefined ? undefined : roster.find((s) => s.seat === ourSeat)
  const duty = mine?.owes
  const owed = duty && duty.kind !== 'action' ? `${duty.kind}:${duty.hand}:${duty.at}` : ''

  // Whose turn it is, by the same test the action bar uses to offer buttons, so
  // the sound and the buttons can never disagree.
  const yours =
    hand && !hand.done && hand.phase === 'betting' && hand.ours && (hand.hole ?? []).length >= 2
      ? `${hand.hand}:${hand.street}:${hand.toAct}`
      : ''

  useEffect(() => {
    if (!hand) return
    const s = seen.current
    const first = !s.primed
    s.primed = true

    // A new hand clears the counters first, whatever phase it arrived in.
    if (hand.hand !== s.hand) {
      s.hand = hand.hand
      s.board = 0
      s.moves = ''
    }

    // The deal is when this seat can read its own cards, which is a later
    // moment than the hand starting and the one worth hearing.
    const hole = hand.hole ?? []
    if (hand.hand !== s.dealt && hole.length >= 2) {
      s.dealt = hand.hand
      if (!first) for (let i = 0; i < hole.length; i++) play('card', i * 0.055)
    }

    const board = hand.board ?? []
    if (board.length > s.board) {
      if (!first) for (let i = 0; i < board.length - s.board; i++) play('card', i * 0.09)
      s.board = board.length
    }

    // What each seat did, from `last` on its chair - the signed action, never a
    // guess from the amounts.
    const chairs = hand.chairs ?? []
    const moves = chairs.map((c) => `${c.seat}=${c.last ?? ''}@${c.total}`).join('|')
    if (moves !== s.moves) {
      const before = new Map(s.moves.split('|').filter(Boolean).map((m) => m.split('=') as [string, string]))
      if (!first) {
        for (const c of chairs) {
          const was = before.get(String(c.seat))
          const now = `${c.last ?? ''}@${c.total}`
          if (was === undefined || was === now) continue
          if (c.last === 'check') play('knock')
          else if (c.last === 'fold') play('fold')
          else if (c.last) play('chip')
        }
      }
      s.moves = moves
    }

    // The hand ending, and only when it ended in this seat's favour by the same
    // arithmetic the status line prints. An absent awards list means this peer
    // does not know yet, which is neither a win nor a loss.
    if (hand.done && hand.hand !== s.won && (hand.awards?.length ?? 0) > 0) {
      s.won = hand.hand
      const paid = chairs.find((c) => c.seat === hand.seat)?.total ?? 0
      const net = (hand.awards?.find((a) => a.seat === hand.seat)?.atoms ?? 0) - paid
      if (net > 0 && !first) play('win')
    }
  }, [hand])

  // Your turn, when turns are minutes apart.
  //
  // One figure always, then a second and a third - and those two only while the
  // tab is hidden, because a player who is looking at the screen has a pulsing
  // plate and a row of buttons telling them already. Three sounds over two
  // minutes, then nothing until the turn changes. There is no fourth: somebody
  // two minutes gone is gone, and the owes cue below is what has news then.
  useEffect(() => {
    if (!yours) return
    play('turn')
    const ids = [45_000, 120_000].map((ms) =>
      window.setTimeout(() => {
        if (document.hidden) play('turn')
      }, ms),
    )
    return () => ids.forEach((id) => window.clearTimeout(id))
  }, [yours])

  // What this game owes the table and has not sent.
  //
  // Only the duties a person cannot discharge by acting - a share, a card key, a
  // shuffle, a checkpoint. Those are the dangerous ones: there is no button for
  // them, so a stuck client looks exactly like a quiet table until somebody
  // proposes taking the bond over it.
  //
  // The wait before the first pulse is deliberately not a chain deadline. It
  // asks "is this ordinary work or is this stuck", which is a question about
  // this machine and can be timed in seconds. How long the other seats have
  // been counting is not knowable here and is not counted here.
  useEffect(() => {
    if (!owed) return
    const ids = [20_000, 60_000, 180_000].map((ms, i) =>
      window.setTimeout(() => {
        if (i === 0 || document.hidden) play('owes')
      }, ms),
    )
    return () => ids.forEach((id) => window.clearTimeout(id))
  }, [owed])
}
