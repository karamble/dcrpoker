// Where a seat sits, as a fraction of the table.
//
// Ours is position 0, the bottom, which is where every poker player on earth
// expects to be. Sizes 2..6, escrow.MaxMembers.
//
// These were twenty rules in the stylesheet, one per (table size, position),
// written that way for a strict style-src that no longer exists. The numbers
// have not changed. What moving them buys is that they can now be arithmetic:
// the vector a card is dealt along and the line a chip takes to the pot are
// both differences between two of these, and a stylesheet could give neither.

export type Spot = { left: number; top: number }

export const seatAt: Record<number, Spot[]> = {
  2: [
    { left: 50, top: 84 },
    { left: 50, top: 13 },
  ],
  3: [
    { left: 50, top: 84 },
    { left: 15, top: 26 },
    { left: 85, top: 26 },
  ],
  4: [
    { left: 50, top: 84 },
    { left: 12, top: 45 },
    { left: 50, top: 13 },
    { left: 88, top: 45 },
  ],
  5: [
    { left: 50, top: 84 },
    { left: 13, top: 58 },
    { left: 22, top: 17 },
    { left: 78, top: 17 },
    { left: 87, top: 58 },
  ],
  6: [
    { left: 50, top: 84 },
    { left: 14, top: 66 },
    { left: 14, top: 24 },
    { left: 50, top: 13 },
    { left: 86, top: 24 },
    { left: 86, top: 66 },
  ],
}

export const betAt: Record<number, Spot[]> = {
  2: [
    { left: 50, top: 62 },
    { left: 50, top: 30 },
  ],
  3: [
    { left: 50, top: 62 },
    { left: 27, top: 39 },
    { left: 73, top: 39 },
  ],
  4: [
    { left: 50, top: 62 },
    { left: 26, top: 45 },
    { left: 50, top: 30 },
    { left: 74, top: 45 },
  ],
  5: [
    { left: 50, top: 62 },
    { left: 27, top: 52 },
    { left: 33, top: 32 },
    { left: 67, top: 32 },
    { left: 73, top: 52 },
  ],
  6: [
    { left: 50, top: 62 },
    { left: 28, top: 56 },
    { left: 28, top: 36 },
    { left: 50, top: 30 },
    { left: 72, top: 36 },
    { left: 72, top: 56 },
  ],
}

/** The middle of the felt, matching .center in felt.css. Cards come from here
 *  and chips go here, so it has to be the same point the pot is drawn at. */
export const centre: Spot = { left: 50, top: 44 }

/** spotFor is the position with a fallback, because a roster longer than six
 *  seats would be a protocol change rather than a layout problem, and a felt
 *  that threw would be a worse way to find out. */
export function spotFor(table: Record<number, Spot[]>, size: number, pos: number): Spot {
  const row = table[size] ?? table[6]
  return row[pos] ?? centre
}
