import type { Snapshot } from '../api'

/** who is what to call a table.
 *
 *  The protocol does not name them, so the honest label is who else is at it.
 *  A name is a label for a chair and never an identity - the sid stays on the
 *  row underneath, and nothing here decides anything by name. */
export function who(t: Snapshot): string {
  const named: string[] = []
  for (const s of t.roster ?? []) if (!s.ours && s.name) named.push(s.name)
  if (named.length === 0) return `${t.seats} seats`
  if (named.length <= 2) return named.join(' and ')
  return `${named[0]}, ${named[1]} and ${named.length - 2} more`
}
