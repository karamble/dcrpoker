// The four suits, drawn rather than typed.
//
// No font this ships carries them. Both faces are latin subsets and both stop
// well before U+2660, so a ♠ set in them comes from whatever the machine
// happened to have installed - which means no two players see the same card,
// on a table whose whole argument is that you do not have to take anybody's
// word for what you were dealt.
//
// Drawn, they are also the same shape at 13px in a plate and at 46px behind a
// rank, and they take the card's own colour without a second rule.

const paths: Record<string, string> = {
  s: 'M12 2.6C12 2.6 4.6 8.4 4.6 13.2a4.05 4.05 0 0 0 6.6 3.15c-.1 2.2-.9 3.85-2.35 5.05h6.3c-1.45-1.2-2.25-2.85-2.35-5.05a4.05 4.05 0 0 0 6.6-3.15C19.4 8.4 12 2.6 12 2.6Z',
  h: 'M12 21.2C12 21.2 2.6 15.4 2.6 9.6A5.2 5.2 0 0 1 12 7.4a5.2 5.2 0 0 1 9.4 2.2c0 5.8-9.4 11.6-9.4 11.6Z',
  d: 'M12 2.2 20.2 12 12 21.8 3.8 12Z',
  c: 'M12 2.2a3.6 3.6 0 0 0-2.9 5.75 3.7 3.7 0 1 0-1.05 7.3 3.66 3.66 0 0 0 2.85-1.4c-.12 2.4-.9 4.2-2.4 5.55h6.99c-1.5-1.35-2.28-3.15-2.4-5.55a3.66 3.66 0 0 0 2.85 1.4 3.7 3.7 0 1 0-1.05-7.3A3.6 3.6 0 0 0 12 2.2Z',
}

/** suitOf reads the letter off a card as the plugin writes it - "As", "Td",
 *  "9c". Kept here rather than taken from cardParts, which returns the glyph
 *  and is shared with code that still wants one. */
export function suitOf(card: string): string {
  return card.slice(-1).toLowerCase()
}

export function Pip({ suit, size = 16, className }: { suit: string; size?: number; className?: string }) {
  const d = paths[suit]
  if (!d) return null
  return (
    <svg
      viewBox="0 0 24 24"
      width={size}
      height={size}
      className={className}
      aria-hidden="true"
      focusable="false"
    >
      <path d={d} fill="currentColor" />
    </svg>
  )
}
