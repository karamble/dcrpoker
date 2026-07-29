// What the plugin says, and how to ask it.
//
// Hand-written rather than generated. There are about thirty fields, the Go
// side pins their names in a test, and a code generator would be a build step
// and a dependency in a bundle that deliberately has neither.
//
// Every path here is relative. The document is served at <prefix>/ui/ and the
// host mounts <prefix> wherever it likes, so `../tables` is the only form that
// works - and it means the request goes back through the proxy that swaps this
// page's short-lived token for the plugin's real one. Nothing here ever holds
// the plugin's own credentials.

export type Duty = {
  seat: number
  kind: 'cardkey' | 'shuffle' | 'share' | 'action' | 'checkpoint'
  hand: number
  at: number
}

export type SeatView = {
  seat: number
  ours?: boolean
  key?: string
  /** An outpoint this peer found on the chain. Absent means it has not seen
   *  the money, which is not the same as the money not being there. */
  stake?: string
  bond?: string
  bondAt?: string
  /** What this seat announced that this peer has not accepted, and how far off
   *  it is. An absent outpoint with one of these is money on its way; an absent
   *  outpoint with neither is money nobody here has heard of. */
  stakeWait?: Waiting
  bondWait?: Waiting
  payout?: string
  leaving?: boolean
  owes?: Duty
  says?: string
}

export type Boundary = { hand: number; stacks: number[] }

export type Snapshot = {
  sid: string
  gcid: string
  state: 'joining' | 'formed' | 'committed' | 'settled' | 'aborted' | 'unknown'
  seats: number
  buyinAtoms: number
  csvBlocks: number
  joined: number
  matchId?: string
  reason?: string
  /** The height admission closed at, which is what tables are ordered by. */
  until?: number
  /** The block the seating is drawn from. Given rather than derived, so this
   *  page never carries its own copy of a protocol constant. */
  seatsAt?: number
  waiting: number
  funded: number
  seat?: number
  depositAddr?: string
  stake?: string
  fundingDeadline?: number
  bonded: number
  dealing: boolean
  over?: boolean
  hand?: number
  settled?: Boundary
  live?: number[]
  height?: number
  payoutSet: boolean
  roster?: SeatView[]
  /** This player has left and the table is kept only as a record of coin still
   *  on the chain. Nothing is dealt at it and nobody is seated again. */
  finished?: boolean
}

export type Waiting = {
  outpoint: string
  confirmations: number
  needs: number
  where: 'absent' | 'mempool' | 'confirming'
}

export type Shuffle = { seat: number; state: 'ours' | 'verified' | 'awaited' }

export type Award = { seat: number; atoms: number }

/** Chair is one seat's position in the hand being played, which is not the
 *  stack it has signed for at the last boundary. `committed` is the chips in
 *  front of the seat on this street — still theirs until the street ends,
 *  because an uncalled bet has to be returnable. */
export type Chair = {
  seat: number
  stack: number
  committed: number
  total: number
  folded?: boolean
  allIn?: boolean
}

export type HandView = {
  sid: string
  match: string
  seat: number
  hand: number
  phase: string
  street: string
  toAct: number
  ours: boolean
  toCall: number
  minRaise: number
  legal?: string[]
  hole?: string[]
  board?: string[]
  pot: number
  chairs?: Chair[]
  stacks: number[]
  shuffles?: Shuffle[]
  done: boolean
  awards?: Award[]
  settled?: Boundary
}

export type ChainEvent = {
  at: number
  kind:
    | 'proposed'
    | 'cosigned'
    | 'refused'
    | 'claimed'
    | 'answered'
    | 'unanswerable'
    | 'settled'
    | 'blocked'
  text: string
  txid?: string
  seat?: number
}

export type ClaimView = {
  duty: Duty
  says: string
  against: number
  ours?: boolean
  mine?: boolean
  signed: number
  needs: number
  done?: boolean
  bond?: string
  atomsEach?: number
}

export type SettleView = {
  hand: number
  signed?: number[]
  needs: number
  done?: boolean
  pays?: { seat: number; atoms: number; address?: string }[]
  blocked?: string
}

export type LedgerView = {
  sid: string
  match?: string
  height?: number
  settled?: Boundary
  live?: number[]
  roster?: SeatView[]
  payoutsMissing?: number[]
  refreshed: boolean
  claims?: ClaimView[]
  settlement?: SettleView
  events?: ChainEvent[]
}

/** The token the host handed this page over its message port, and the only
 *  credential it ever has. Kept in a module variable rather than storage: an
 *  opaque origin has no useful storage anyway, and a token that outlived the
 *  panel would be a token nobody revoked. */
let token = ''
let base = '..'

export function configure(next: { token: string; apiBase?: string }) {
  token = next.token
  if (next.apiBase) base = next.apiBase.replace(/\/+$/, '')
}

export function haveToken() {
  return token !== ''
}

/** authToken is read at request time rather than captured, because the host
 *  rotates it while the panel is open and a value taken when a long-lived
 *  connection opened would be the old one. */
export function authToken() {
  return token
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message)
  }
}

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${base}/${path}`, {
    ...init,
    headers: {
      ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...init?.headers,
    },
  })
  const text = await res.text()
  if (!res.ok) {
    let message = text
    try {
      const parsed = JSON.parse(text)
      if (parsed && typeof parsed.error === 'string') message = parsed.error
    } catch {
      // Not JSON. The status and the body are what there is.
    }
    throw new ApiError(res.status, message || res.statusText)
  }
  return text ? (JSON.parse(text) as T) : (null as T)
}

/** callText fetches something meant to be kept rather than read - a log a
 *  person saves - so it comes back as the bytes the plugin sent. */
async function callText(path: string): Promise<string> {
  const res = await fetch(`${base}/${path}`, {
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  })
  const text = await res.text()
  if (!res.ok) {
    let message = text
    try {
      const parsed = JSON.parse(text)
      if (parsed && typeof parsed.error === 'string') message = parsed.error
    } catch {
      // Not JSON. The status and the body are what there is.
    }
    throw new ApiError(res.status, message || res.statusText)
  }
  return text
}

export const api = {
  tables: () => call<{ tables: Snapshot[] }>('tables'),
  hand: (sid: string) => call<HandView>(`table/hand?sid=${encodeURIComponent(sid)}`),
  ledger: (sid: string) => call<LedgerView>(`table/ledger?sid=${encodeURIComponent(sid)}`),
  act: (sid: string, action: string, amount: number) =>
    call<HandView>('table/act', {
      method: 'POST',
      body: JSON.stringify({ sid, action, amount }),
    }),
  leave: (sid: string) =>
    call<{ left: boolean }>('table/leave', {
      method: 'POST',
      body: JSON.stringify({ sid }),
    }),
  // The three below are detached deliberately. Each asks the host for a
  // payment that a person then approves in the dashboard, which takes as long
  // as it takes - up to half an hour for a bond. A page cannot hold a request
  // open for that, so it gets a spend id back at once and learns the outcome
  // the same way it learns everything else: the table changes.
  fund: (sid: string) =>
    call<Asked>('table/fund', {
      method: 'POST',
      body: JSON.stringify({ sid, detach: true }),
    }),
  bondTable: (sid: string) =>
    call<Asked>('table/bond', {
      method: 'POST',
      body: JSON.stringify({ sid, detach: true }),
    }),
  bond: () => call<Bond>('bond'),
  /** The signed log this table was played from. Not parsed here - it is handed
   *  to a person to keep, and the point of it is that somebody who was not
   *  there and trusts nobody who was can check it. */
  log: (sid: string) => callText(`table/log?sid=${encodeURIComponent(sid)}`),
  fundBond: () =>
    call<Asked>('bond/fund', {
      method: 'POST',
      body: JSON.stringify({ detach: true }),
    }),
  spend: (id: string) => call<Spend>(`spend?id=${encodeURIComponent(id)}`),
}

/** Bond is the standing deposit that makes a seat cost something.
 *
 *  It is not a table's bond: this one is posted once, buys the right to join
 *  anything, and is deliberately not forfeitable. The chain fields are absent
 *  until it has been paid, and `chainErr` says the host's node could not be
 *  asked - which is not the same as the bond not being there. */
export type Bond = {
  address: string
  scriptHex: string
  outpoint: string
  minAtoms: number
  minBlocks: number
  confsNeed: number
  hasDeposit: boolean
  atoms?: number
  confirmations?: number
  height?: number
  maturesAt?: number
  blocksLeft?: number
  spendable?: boolean
  spent?: boolean
  chainErr?: string
}

/** Asked is what comes back from a payment nobody waited for. */
export type Asked = {
  seat?: number
  spendId: string
  state: string
}

/** Spend merges the host's view with what the plugin did about it. Approved and
 *  recorded against a seat are different facts, and the gap between them is
 *  exactly where a payment gets lost. */
export type Spend = {
  id: string
  purpose: string
  sid?: string
  seat: number
  state: string
  txid?: string
  outpoint?: string
  recorded: boolean
  error?: string
}

/** eventsUrl is the stream, with the token in the query because EventSource
 *  cannot set headers. Not used - see stream() - and kept here only so the
 *  reason it is not used sits beside the thing it is about: a token in a URL
 *  ends up in history and in every log between here and the plugin. */
export function streamPath() {
  return `${base}/events`
}
