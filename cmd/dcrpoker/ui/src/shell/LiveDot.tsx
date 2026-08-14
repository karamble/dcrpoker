// Whether this page is being told or is asking.
//
// Worth a permanent corner of the screen rather than a warning, because
// "nothing is happening" and "we stopped hearing" look identical at a table and
// are not the same fact. `terse` is for the felt, where the line has cards
// beside it and no room to explain itself.

export function LiveDot({ live, terse }: { live: boolean; terse?: boolean }) {
  return (
    <span className="live">
      <span className={`dot ${live ? 'on' : 'off'}`} />
      {live ? 'live' : terse ? 'polling' : 'asking every 2s'}
    </span>
  )
}
