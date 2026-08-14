import type { Duty, HandView, SeatView, Snapshot } from './api'
import type { Tab } from './route'
import { blocks, dcr } from './format'

// What a table actually is.
//
// Three things are true about a table at once - how far formation got, whether
// the draw has given this player a seat, and whether it is playing - and only
// all three together say anything. Reading `state` alone is what made a dead
// table read "308ceba2 · settled" in the old picker: a table this player has
// left still reports settled, over a funded count the backend cleared once the
// payout confirmed.
//
// So there is one answer, and the words, the colour and the destination all
// come out of it. Nothing else in the lobby reads `state`.

export type Tone = 'hot' | 'owed' | 'waiting' | 'cold' | 'bad'

export type Standing = {
  /** The phrase the row leads with. */
  say: string
  /** A second clause, when the first needs qualifying. For a wait this is
   *  always the block being waited for: a wait's whole content is until-when. */
  note?: string
  tone: Tone
  /** Where pressing the row lands. */
  goto: 'felt' | Tab
  /** True when this table is waiting on this player and on nobody else. */
  yours: boolean
}

/** owed names, shortly and in the second person, what a seat is being waited on
 *  for. A full Record rather than a Partial on purpose: a kind added to the
 *  protocol should fail this build rather than quietly render as a blank. */
const owed: Record<Duty['kind'], string> = {
  cardkey: 'your card key is owed',
  shuffle: 'your shuffle is owed',
  share: 'your share of a card is owed',
  action: 'your turn',
  checkpoint: 'your signature is owed',
  reveal: 'your reveal is owed',
}

/** stalled is somebody holding the table up, which outranks whatever else the
 *  table is doing.
 *
 *  It is the only thing in this list with a price for being ignored: an
 *  obligation that stands long enough is one the other seats may take that
 *  seat's table bond over. The plugin has published it on every seat all along
 *  and nothing rendered it - and it cannot come from the shell here, because
 *  the alarm up there is driven from a table view and the lobby has no sid.
 *
 *  How long it has stood is said in blocks, counted from the height the duty
 *  was incurred at. No countdown and no seconds: every clock in this protocol
 *  is a block height, and the number of blocks it takes is a constant this page
 *  deliberately does not carry a copy of. */
function stalled(t: Snapshot, hand?: HandView): Standing | undefined {
  const owing: SeatView[] = []
  for (const s of t.roster ?? []) if (s.owes) owing.push(s)
  if (owing.length === 0) return undefined

  const mine = t.seat === undefined ? undefined : owing.find((s) => s.seat === t.seat)
  if (!mine?.owes) {
    // Somebody else. Worth saying, because it is why nothing is moving.
    const first = owing[0]
    const name = first.name ?? `seat ${first.seat}`
    return {
      say: owing.length > 1 ? `waiting on ${owing.length} seats` : `waiting on ${name}`,
      note: first.says ?? 'nothing is dealt until it arrives',
      tone: 'waiting',
      goto: 'now',
      yours: false,
    }
  }

  const duty = mine.owes
  const stood = t.height !== undefined && duty.at ? Math.max(0, t.height - duty.at) : undefined
  const since = stood ? `standing ${stood} ${stood === 1 ? 'block' : 'blocks'}` : undefined

  // A duty every seat owes at once is not this player holding anything up.
  // 'reveal' is the whole table being audited: it lands on all of them the
  // moment a hand is challenged, and reading it as "waiting on you" would name
  // somebody for what nobody has failed to do yet.
  const shared = owing.length > 1 && owing.every((s) => s.owes?.kind === duty.kind)
  if (shared) {
    return {
      say: duty.kind === 'reveal' ? 'the hand is being audited' : 'the table is waiting on every seat',
      note: [`every seat owes ${duty.kind === 'reveal' ? 'a reveal' : 'the same thing'}`, since]
        .filter(Boolean)
        .join(' · '),
      tone: 'waiting',
      goto: duty.kind === 'reveal' ? 'audit' : 'now',
      yours: false,
    }
  }

  // This player, alone. Acting keeps the poker wording and the felt, because
  // that is where it is done and the amount to call is the useful part; a
  // block gone by still gets said, because by then it is not just a turn.
  if (duty.kind === 'action') {
    return {
      say: 'your turn',
      note: [hand?.toCall ? `${dcr(hand.toCall)} DCR to call` : undefined, since]
        .filter(Boolean)
        .join(' · '),
      tone: stood ? 'bad' : 'hot',
      goto: 'felt',
      yours: true,
    }
  }

  return {
    say: owed[duty.kind],
    note: since
      ? `${since}; the others may take this seat's bond`
      : 'nothing is dealt until it arrives',
    tone: stood ? 'bad' : 'owed',
    goto: 'now',
    yours: true,
  }
}

export function standing(t: Snapshot, hand?: HandView): Standing {
  const at = t.height

  // Terminal first, and unconditionally. A finished or ended table still
  // reports state:'settled', and the backend clears its funded and bonded
  // counts once the payout confirms - so every predicate below would read
  // not-done again and walk the row backwards to "pay your stake".
  if (t.finished) {
    return {
      say: 'you left',
      note: 'kept while the coin is locked',
      tone: 'cold',
      goto: 'audit',
      yours: false,
    }
  }
  if (t.over) {
    return {
      say: 'ended',
      note: 'the payout is on its way to the chain',
      tone: 'cold',
      goto: 'audit',
      yours: false,
    }
  }
  if (t.state === 'aborted') {
    return { say: 'ended before it dealt', note: t.reason, tone: 'bad', goto: 'now', yours: false }
  }
  if (t.state === 'unknown') {
    return { say: 'in a state this build does not know', tone: 'bad', goto: 'now', yours: false }
  }

  // Before anything about what the table is doing: a duty outstanding is worth
  // more than a street, and it is the one thing here that can cost coin.
  const stall = stalled(t, hand)
  if (stall) return stall

  if (t.dealing) {
    if (hand?.ours && hand.phase === 'betting') {
      return {
        say: 'your turn',
        note: hand.toCall > 0 ? `${dcr(hand.toCall)} DCR to call` : 'nothing to call',
        tone: 'hot',
        goto: 'felt',
        yours: true,
      }
    }
    return {
      say: `hand ${hand?.hand ?? t.hand ?? 1}`,
      note: hand?.phase === 'betting' ? hand.street : hand?.phase,
      tone: 'hot',
      goto: 'felt',
      yours: false,
    }
  }

  // settled is two entirely different situations, and the difference is whether
  // the draw has handed this player a seat. Without one, fund and bondTable are
  // both refused and there is nothing anybody can press.
  if (t.state === 'settled') {
    if (t.seat === undefined) {
      return {
        say: 'waiting for the seating',
        note: t.seatsAt
          ? `drawn from block ${t.seatsAt.toLocaleString()} · ${blocks(t.seatsAt, at)}`
          : undefined,
        tone: 'waiting',
        goto: 'now',
        yours: false,
      }
    }
    const ours = (t.roster ?? []).find((s) => s.ours)
    if (!ours?.stake) {
      return {
        say: 'your stake is owed',
        note: t.fundingDeadline
          ? `by block ${t.fundingDeadline.toLocaleString()} · ${blocks(t.fundingDeadline, at)}`
          : `${dcr(t.buyinAtoms)} DCR`,
        tone: 'owed',
        goto: 'money',
        yours: true,
      }
    }
    if (!ours.bond && !ours.bondAt) {
      return {
        say: 'your table bond is owed',
        note: 'the second payment, and a different thing',
        tone: 'owed',
        goto: 'money',
        yours: true,
      }
    }
    if (t.funded < t.seats || t.bonded < t.seats) {
      return {
        say: 'waiting for the other seats',
        note: `${t.funded}/${t.seats} staked · ${t.bonded}/${t.seats} bonded`,
        tone: 'waiting',
        goto: 'now',
        yours: false,
      }
    }
    return { say: 'about to deal', tone: 'waiting', goto: 'now', yours: false }
  }

  // joining, formed, committed. Full-but-unconfirmed is worth naming on its
  // own: it is what a join that never arrived looks like from here.
  if (t.joined >= t.seats && t.commits < t.seats) {
    return {
      say: 'full, not confirmed by every seat',
      note: `${t.commits} of ${t.seats} have bound themselves`,
      tone: 'waiting',
      goto: 'now',
      yours: false,
    }
  }
  return {
    say: 'filling',
    note: t.until
      ? `closes at block ${t.until.toLocaleString()} · ${blocks(t.until, at)}`
      : `${t.joined} of ${t.seats} seats`,
    tone: 'waiting',
    goto: 'now',
    yours: false,
  }
}
