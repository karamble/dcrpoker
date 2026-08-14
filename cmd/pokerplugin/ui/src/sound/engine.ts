// The table's own noises, made rather than recorded.
//
// Nothing here is an asset. A binary that gets signed should carry as little
// that cannot be read as possible, and forty lines of oscillator is a thing a
// reviewer can evaluate where fifty kilobytes of base64 is not. It also means
// no codec, no second format for one browser, and no licence to carry.
//
// The ceiling this imposes is the ceiling we want. Synthesis cannot fake a clay
// chip riffle or a shuffle rasp, and a table playing for real coin should not
// sound like a machine that wants you to keep pressing it.
//
// Everything is short and quiet: peaks between 0.045 and 0.10 against a master
// of 0.9. This is a table somebody sits at for an hour.

export type Cue = 'card' | 'chip' | 'knock' | 'fold' | 'turn' | 'win' | 'refuse' | 'owes'

/** Cues worth hearing when this is not the tab being looked at.
 *
 *  Turns here are minutes apart, so reaching somebody who went elsewhere is the
 *  whole reason a sound exists. Keeping the rest silent is for the same person:
 *  a background tab clattering about other people's chips is rude. */
const whileHidden = new Set<Cue>(['turn', 'owes'])

const KEY = 'poker.sound'

let ctx: AudioContext | undefined
let master: GainNode | undefined
let noise: AudioBuffer | undefined
let muted = remembered()
let armed = false
const lastAt = new Map<Cue, number>()

function remembered(): boolean {
  try {
    return window.localStorage.getItem(KEY) === 'off'
  } catch {
    // Storage throws when it is disabled. Remembering is a nicety, and on is
    // both the default and the right way to fail.
    return false
  }
}

/** start builds the context, or wakes one the browser suspended.
 *
 *  A page cannot make a sound before somebody has touched it. That is the
 *  browser's rule and it is a good one, so this is called from a gesture rather
 *  than worked around. */
export function start(): void {
  if (!ctx) {
    const Ctor: typeof AudioContext | undefined =
      window.AudioContext ?? (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext
    if (!Ctor) return
    ctx = new Ctor()
    master = ctx.createGain()
    master.gain.value = 0.9
    master.connect(ctx.destination)
    // One second of white noise, reused with a random offset by everything
    // percussive. Cards and chips are filtered noise, not tones.
    const n = ctx.sampleRate
    noise = ctx.createBuffer(1, n, n)
    const data = noise.getChannelData(0)
    for (let i = 0; i < n; i++) data[i] = Math.random() * 2 - 1
  }
  if (ctx.state === 'suspended') void ctx.resume()
}

/** arm waits for the first thing anybody does.
 *
 *  Any click will do, so somebody who simply plays never learns there was a
 *  step - the sound control is not the only way in. Idempotent, because the
 *  component that calls it mounts and unmounts with the felt. */
export function arm(): void {
  if (armed) return
  armed = true
  const once = { once: true, capture: true, passive: true } as const
  window.addEventListener('pointerdown', start, once)
  window.addEventListener('keydown', start, once)
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) start()
  })
}

/** running says a browser would actually let this page make a noise. Until
 *  somebody has clicked something it will not, which is worth showing rather
 *  than looking broken. */
export function running(): boolean {
  return ctx?.state === 'running'
}

export function isMuted(): boolean {
  return muted
}

export function setMuted(next: boolean): void {
  muted = next
  try {
    window.localStorage.setItem(KEY, next ? 'off' : 'on')
  } catch {
    /* Nothing to do. */
  }
  if (!next) start()
}

// ---- the parts ----

function env(t: number, peak: number, dur: number, attack = 0.0025): GainNode {
  const g = ctx!.createGain()
  g.gain.setValueAtTime(0.0001, t)
  g.gain.linearRampToValueAtTime(peak, t + attack)
  g.gain.exponentialRampToValueAtTime(0.0001, t + dur)
  return g
}

type ToneOpts = {
  type?: OscillatorType
  cutoff?: number
  glideTo?: number
  attack?: number
}

function tone(t: number, freq: number, dur: number, peak: number, opts: ToneOpts = {}): void {
  const o = ctx!.createOscillator()
  o.type = opts.type ?? 'sine'
  o.frequency.setValueAtTime(freq, t)
  if (opts.glideTo) o.frequency.exponentialRampToValueAtTime(opts.glideTo, t + dur)
  let node: AudioNode = o
  if (opts.cutoff) {
    const f = ctx!.createBiquadFilter()
    f.type = 'lowpass'
    f.frequency.value = opts.cutoff
    node.connect(f)
    node = f
  }
  node.connect(env(t, peak, dur, opts.attack)).connect(master!)
  o.start(t)
  o.stop(t + dur + 0.02)
}

function hiss(
  t: number,
  dur: number,
  peak: number,
  kind: BiquadFilterType,
  from: number,
  to: number,
  q = 0.7,
): void {
  const s = ctx!.createBufferSource()
  s.buffer = noise!
  s.loop = true
  const f = ctx!.createBiquadFilter()
  f.type = kind
  f.Q.value = q
  f.frequency.setValueAtTime(from, t)
  if (to !== from) f.frequency.exponentialRampToValueAtTime(to, t + dur)
  s.connect(f).connect(env(t, peak, dur)).connect(master!)
  s.start(t, Math.random() * 0.5)
  s.stop(t + dur + 0.02)
}

// ---- the voices ----

const voices: Record<Cue, (t: number) => void> = {
  // A card touching felt: noise with the filter falling away under it.
  card: (t) => hiss(t, 0.026, 0.06, 'bandpass', 3200, 1400),

  // Clay, not coins. Three dry clicks, scattered so no two are the same.
  chip: (t) => {
    const at = [0, 0.026, 0.049]
    for (let i = 0; i < at.length; i++) {
      hiss(t + at[i], 0.018, 0.05 - i * 0.008, 'bandpass', 3600 * (0.92 + Math.random() * 0.16), 3600, 4)
    }
  },

  // Players knock the table to check, so that is what checking sounds like.
  knock: (t) => {
    tone(t, 96, 0.07, 0.1, { glideTo: 62 })
    hiss(t, 0.035, 0.05, 'lowpass', 420, 420)
  },

  fold: (t) => hiss(t, 0.13, 0.045, 'lowpass', 1500, 380),

  // A fifth. Attention, not reward.
  turn: (t) => {
    tone(t, 523.25, 0.17, 0.075, { type: 'triangle', cutoff: 2600 })
    tone(t + 0.085, 783.99, 0.22, 0.075, { type: 'triangle', cutoff: 2600 })
  },

  // The same fifth with its octave: the plainest resolved figure there is, on
  // sines, with a long tail. A hand has ended and the log says who took it.
  // Nothing has settled on the chain, so nothing here celebrates.
  win: (t) => {
    tone(t, 523.25, 0.42, 0.07)
    tone(t + 0.1, 783.99, 0.42, 0.07)
    tone(t + 0.2, 1046.5, 0.46, 0.06)
  },

  refuse: (t) => {
    tone(t, 196, 0.06, 0.045, { type: 'square', cutoff: 700 })
    tone(t + 0.06, 164.81, 0.07, 0.045, { type: 'square', cutoff: 700 })
  },

  // Low, slow, and slightly out of tune with itself, so the two notes beat
  // against each other. It should sound like something in the room being wrong
  // rather than like an alarm going off: there is half an hour to fix it.
  owes: (t) => {
    for (const at of [0, 0.46]) {
      tone(t + at, 174.61, 0.7, 0.085, { attack: 0.08 })
      tone(t + at, 176.0, 0.7, 0.055, { attack: 0.08 })
    }
  },
}

/** play makes one sound, if there is any reason to.
 *
 *  offset schedules ahead of now, which is how a row of cards is dealt - one
 *  call per card at a fixed spacing, rather than a timer per card. */
export function play(cue: Cue, offset = 0): void {
  if (muted) return
  if (document.hidden && !whileHidden.has(cue)) return
  start()
  if (!ctx || ctx.state !== 'running' || !master) return
  const now = ctx.currentTime
  // The same cue twice inside a frame is a render artefact, not two events.
  // Per cue rather than overall, so a fold and a chip in one update are both
  // heard - they are two seats, and swallowing one would misreport the table.
  if (offset === 0 && now - (lastAt.get(cue) ?? -1) < 0.03) return
  lastAt.set(cue, now)
  voices[cue](now + offset)
}
