import { useEffect, useState } from 'react'
import { arm, isMuted, running, setMuted, start } from '../sound/engine'

// One switch, at the edge of the felt.
//
// No volume. A table has one useful question about sound, and a slider is a
// second thing to get wrong in a panel where the important controls are the
// ones that move money.
//
// It sits over the felt rather than in the action bar on purpose: nothing here
// should be next to Fold.
//
// The dimmed third state is honest rather than decorative. A browser will not
// let a page make a noise until somebody has touched it, so until then sound is
// on and silent. Pressing this counts as touching it, which makes the state its
// own cure.

export function SoundToggle() {
  const [off, setOff] = useState(isMuted)
  const [live, setLive] = useState(running)

  useEffect(() => {
    arm()
  }, [])

  useEffect(() => {
    if (off || live) return
    const id = window.setInterval(() => {
      if (running()) setLive(true)
    }, 500)
    return () => window.clearInterval(id)
  }, [off, live])

  const label = off
    ? 'sound is off'
    : live
      ? 'sound is on'
      : 'sound is on, and starts when you click anything'

  return (
    <button
      type="button"
      className={`sound${off ? ' off' : ''}${!off && !live ? ' waiting' : ''}`}
      aria-pressed={!off}
      aria-label={label}
      title={label}
      onClick={() => {
        start()
        setMuted(!off)
        setOff(!off)
        setLive(running())
      }}
    >
      <svg viewBox="0 0 24 24" width="16" height="16" aria-hidden="true" focusable="false">
        <path d="M4 9.2h3.6L12 5.2v13.6L7.6 14.8H4z" fill="currentColor" />
        {off ? (
          <path
            d="M16.2 9.4l5 5.2M21.2 9.4l-5 5.2"
            stroke="currentColor"
            strokeWidth="1.8"
            strokeLinecap="round"
            fill="none"
          />
        ) : (
          <path
            d="M16.2 9.6a3.6 3.6 0 0 1 0 4.8M18.8 7.2a7.2 7.2 0 0 1 0 9.6"
            stroke="currentColor"
            strokeWidth="1.6"
            strokeLinecap="round"
            fill="none"
          />
        )}
      </svg>
    </button>
  )
}
