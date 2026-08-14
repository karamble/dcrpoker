import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import { bootstrapSession } from './session'
import './app.css'

// Before anything renders. The page starts asking on its first render, so a
// token settled in an effect arrives after the first request has already gone
// out unauthenticated and been answered 404.
bootstrapSession()

const root = document.getElementById('root')
if (!root) throw new Error('the document has no root to mount on')

createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
