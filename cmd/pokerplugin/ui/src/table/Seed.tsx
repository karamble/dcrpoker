import { useState } from 'react'
import { api } from '../api'

/** The seed, and the one place a person can see it.
 *
 *  It is the whole of this player: every table key, every bond key and every
 *  log key is derived from it, so a machine that loses it loses the coin its
 *  escrows are holding, and no other party can help. Not the other players -
 *  their signatures are on branches that need this key too - and not the
 *  dashboard, which has never held a copy and has no call in the contract that
 *  would let it ask for one.
 *
 *  So this is not a convenience. It is the only exit, and it is deliberately
 *  here rather than anywhere the bridge can reach: the person who needs it is
 *  already sitting in front of this page.
 *
 *  It used to render in browser defaults - `.seed`, `.error` and `.seed-shown`
 *  matched no rule anywhere, a bare h3 sat where every other card has an h2,
 *  and the two buttons were wrapped in `.row`, whose space-between threw them
 *  to opposite ends of the card. The only exit from a lost machine should not
 *  be the one thing on screen that looks unfinished. */
export function Seed() {
  const [seed, setSeed] = useState<string | null>(null)
  const [bond, setBond] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [kept, setKept] = useState(false)

  const reveal = async () => {
    setBusy(true)
    setError(null)
    try {
      const got = await api.seedBackup()
      setSeed(got.seedHex)
      setBond(got.bondOutpoint)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'could not read the seed')
    } finally {
      setBusy(false)
    }
  }

  // Acknowledging and hiding are one act. Leaving it on screen after the
  // person has said they copied it only makes it easier to photograph.
  const acknowledge = async () => {
    setBusy(true)
    try {
      await api.seedAcknowledge()
      setKept(true)
      setSeed(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'could not record that')
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="card grave">
      <h2>This player's seed</h2>
      <p className="lede">
        Every key this game holds comes from these 32 bytes. Written down and kept somewhere
        else, they are how you recover a stake or a bond if this machine is lost. Nobody can
        reissue them for you - not the other players, and not the dashboard, which has never
        had a copy.
      </p>

      {error && <p className="lede bad">{error}</p>}

      {seed === null && !kept && (
        <div className="actions">
          <button type="button" className="act" onClick={reveal} disabled={busy}>
            {busy ? 'reading…' : 'Show my seed'}
          </button>
        </div>
      )}

      {kept && <p className="lede muted">Backed up. Nothing will ask again.</p>}

      {seed !== null && (
        <div className="rows">
          <p className="lede warn">
            Anyone who reads this can spend everything this player holds. Copy it somewhere
            offline, then close it.
          </p>
          <textarea
            className="seed-hex mono"
            readOnly
            rows={3}
            spellCheck={false}
            autoComplete="off"
            value={seed}
            aria-label="This player's seed, in hex"
            onFocus={(e) => e.currentTarget.select()}
          />
          {bond && (
            <p className="lede muted">
              Bond outpoint <span className="mono">{bond}</span> - keep this with the seed. It
              is where the fidelity bond is, and a restored player needs it to find it again.
            </p>
          )}
          <div className="actions">
            <button type="button" className="act primary" onClick={acknowledge} disabled={busy}>
              I have written it down
            </button>
            <button type="button" className="act" onClick={() => setSeed(null)} disabled={busy}>
              Hide
            </button>
          </div>
        </div>
      )}
    </section>
  )
}
