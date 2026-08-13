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
 *  already sitting in front of this page. */
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
    <section className="seed">
      <h3>This player's seed</h3>
      <p className="muted">
        Every key this game holds comes from these 32 bytes. Written down and kept somewhere
        else, they are how you recover a stake or a bond if this machine is lost. Nobody can
        reissue them for you - not the other players, and not the dashboard, which has never
        had a copy.
      </p>

      {error && <p className="error">{error}</p>}

      {seed === null && !kept && (
        <button type="button" onClick={reveal} disabled={busy}>
          {busy ? 'Reading...' : 'Show my seed'}
        </button>
      )}

      {kept && <p className="muted">Backed up. Nothing will ask again.</p>}

      {seed !== null && (
        <div className="seed-shown">
          <p className="warn">
            Anyone who reads this can spend everything this player holds. Copy it somewhere
            offline, then close it.
          </p>
          <textarea readOnly rows={3} value={seed} onFocus={(e) => e.currentTarget.select()} />
          {bond && (
            <p className="muted">
              Bond outpoint <span className="mono">{bond}</span> - keep this with the seed. It
              is where the fidelity bond is, and a restored player needs it to find it again.
            </p>
          )}
          <div className="row">
            <button type="button" onClick={acknowledge} disabled={busy}>
              I have written it down
            </button>
            <button type="button" onClick={() => setSeed(null)} disabled={busy}>
              Hide
            </button>
          </div>
        </div>
      )}
    </section>
  )
}
