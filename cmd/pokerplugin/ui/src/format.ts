// Saying numbers and hashes in a way somebody can check.

const ATOMS = 100_000_000

/** dcr renders atoms as DCR. Trailing zeros are kept to four places so a
 *  column of them lines up and two amounts can be compared by looking. */
export function dcr(atoms: number | undefined): string {
  if (atoms === undefined) return '—'
  const sign = atoms < 0 ? '-' : ''
  const abs = Math.abs(atoms)
  const whole = Math.floor(abs / ATOMS)
  const frac = String(abs % ATOMS).padStart(8, '0').slice(0, 4)
  return `${sign}${whole.toLocaleString()}.${frac}`
}

/** atoms parses what somebody typed as DCR back into atoms.
 *
 *  Everything a person sees here is DCR, so everything a person types is too.
 *  The protocol counts in atoms and the conversion belongs at this edge and
 *  nowhere else - an interface that showed one unit and accepted another would
 *  be inviting a bet a hundred million times the intended size. */
export function atoms(dcrText: string): number {
  const n = Number(dcrText)
  if (!Number.isFinite(n) || n < 0) return 0
  return Math.round(n * ATOMS)
}

/** delta renders a difference, with its sign, because the sign is the whole
 *  point of showing one. */
export function delta(atoms: number): string {
  if (atoms === 0) return '±0'
  return `${atoms > 0 ? '+' : ''}${dcr(atoms)}`
}

/** short abbreviates a hash or an outpoint to something a person can compare
 *  against a block explorer without reading sixty-four characters. Both ends,
 *  never one: a prefix alone is cheap to collide on deliberately. */
export function short(id: string | undefined, keep = 6): string {
  if (!id) return ''
  const [hash, index] = id.split(':')
  const body =
    hash.length <= keep * 2 + 1 ? hash : `${hash.slice(0, keep)}…${hash.slice(-keep)}`
  return index === undefined ? body : `${body}:${index}`
}

/** blocks says how far away a height is, in a unit somebody can act on. Blocks
 *  are about five minutes apart on this chain, and that is worth saying because
 *  a block count means nothing to most people and "about an hour" means
 *  something to everybody. */
export function blocks(target: number | undefined, now: number | undefined): string {
  if (!target) return ''
  if (!now) return `block ${target.toLocaleString()}`
  const away = target - now
  if (away <= 0) return 'passed'
  const minutes = away * 5
  let rough: string
  if (minutes < 60) rough = `about ${Math.max(1, Math.round(minutes))} min`
  else if (minutes < 48 * 60) rough = `about ${Math.round(minutes / 60)} h`
  else rough = `about ${Math.round(minutes / (24 * 60))} days`
  return `${away.toLocaleString()} blocks, ${rough}`
}

/** cardParts splits a two or three character card into its rank and suit. The
 *  plugin renders cards as e.g. "As", "Td", "9c". */
export function cardParts(card: string): { rank: string; suit: string; red: boolean } {
  const suit = card.slice(-1).toLowerCase()
  const rank = card.slice(0, -1).toUpperCase()
  const glyph = { s: '♠', h: '♥', d: '♦', c: '♣' }[suit] ?? '?'
  return { rank: rank === 'T' ? '10' : rank, suit: glyph, red: suit === 'h' || suit === 'd' }
}
