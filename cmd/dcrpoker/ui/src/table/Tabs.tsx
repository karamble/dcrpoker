import { useRef } from 'react'
import { go, href, type Tab } from '../route'

// The four questions, as four addresses.
//
// They are real links. The tab is a field in the fragment, so each one has a
// URL that can be copied, opened in another window and reloaded, and a person
// driving this from the keyboard gets Enter for free rather than from a
// handler. The ARIA tab role is what `.tab[aria-selected]` is styled against.
//
// Focus moves with the arrows and activation stays manual, which is the other
// half of that: automatic activation would push a history entry for every key
// press on the way to the tab somebody wanted.

const labels: Record<Tab, string> = {
  now: 'Now',
  money: 'Money',
  roster: 'Roster',
  audit: 'Audit',
}

export const panelId = 'tabpanel'

export function tabId(tab: Tab): string {
  return `tab-${tab}`
}

export function Tabs({ sid, tab, tabs }: { sid: string; tab: Tab; tabs: readonly Tab[] }) {
  const strip = useRef<HTMLDivElement>(null)

  const move = (from: number, by: number) => {
    const at = (from + by + tabs.length) % tabs.length
    strip.current?.querySelectorAll('a')[at]?.focus()
  }

  return (
    <div className="tabs" role="tablist" aria-label="This table" ref={strip}>
      {tabs.map((t, i) => (
        <a
          key={t}
          id={tabId(t)}
          className="tab tablink"
          role="tab"
          href={href({ view: 'table', sid, tab: t })}
          aria-selected={t === tab}
          aria-controls={panelId}
          tabIndex={t === tab ? 0 : -1}
          onClick={(e) => {
            // A modified click is somebody asking for another window, and the
            // fragment they get is the one in the href.
            if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return
            e.preventDefault()
            go({ view: 'table', sid, tab: t })
          }}
          onKeyDown={(e) => {
            if (e.key === 'ArrowRight') move(i, 1)
            else if (e.key === 'ArrowLeft') move(i, -1)
            else if (e.key === 'Home') move(0, 0)
            else if (e.key === 'End') move(tabs.length - 1, 0)
            else if (e.key === ' ') go({ view: 'table', sid, tab: t })
            else return
            e.preventDefault()
          }}
        >
          {labels[t]}
        </a>
      ))}
    </div>
  )
}
