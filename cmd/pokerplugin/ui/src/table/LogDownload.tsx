import { Download } from 'lucide-react'
import { useState } from 'react'
import { api } from '../api'

/** LogDownload hands over the signed log this table was played from.
 *
 *  It is offered wherever the record is being read rather than only at the end,
 *  because it is the thing that needs nobody to be believed: every entry carries
 *  the signature of the seat that made it and the entries chain forward, so a
 *  dispute is argued from this file and not from anybody's account of it.
 *
 *  The bytes are handed over exactly as the plugin sent them. Nothing is parsed,
 *  reformatted or pretty-printed on the way, because a re-encoded log is a
 *  different file and the signatures are over the original. */
export function LogDownload({ sid }: { sid: string }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()

  const save = () => {
    setBusy(true)
    setError(undefined)
    api
      .log(sid)
      .then((text) => {
        const url = URL.createObjectURL(new Blob([text], { type: 'application/json' }))
        const link = document.createElement('a')
        link.href = url
        link.download = `poker-${sid}.json`
        link.click()
        URL.revokeObjectURL(url)
      })
      .catch((e) => setError(String(e instanceof Error ? e.message : e)))
      .finally(() => setBusy(false))
  }

  return (
    <section className="card quiet">
      <h2>The signed log</h2>
      <div className="row">
        <span>Everything this table did, as it was signed</span>
        <button className="act" disabled={busy} onClick={save}>
          {busy ? 'fetching…' : 'Save the log'}
          <Download size={16} strokeWidth={1.75} aria-hidden />
        </button>
      </div>
      <p className="lede muted">
        Every entry carries the signature of the seat that made it and the chain hashes
        forward, so somebody who was not here and trusts nobody who was can check it.
      </p>
      {error && <p className="lede bad">{error}</p>}
    </section>
  )
}
