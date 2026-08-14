// What went wrong with asking, said once and in one place.
//
// It sits above whatever is on screen rather than inside a card, because a
// failure to reach the plugin makes every card below it stale at the same time
// and saying so five times would not make it clearer.

export function ErrorBanner({ error }: { error?: string }) {
  if (!error) return null
  return (
    <div className="banner" role="status">
      {error}
    </div>
  )
}
