import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { viteSingleFile } from 'vite-plugin-singlefile'

// One file, and relative paths inside it.
//
// Two constraints, both from where this ends up rather than from taste.
//
// The page is framed with an opaque origin - `sandbox="allow-scripts"` and
// deliberately not `allow-same-origin` - so every request it makes is
// cross-origin. Vite's normal output is `<script type="module">`, and a module
// script is always fetched in CORS mode: from an opaque origin that needs the
// host to answer with the right headers on every asset, and the credentials it
// would otherwise send are not sent at all. Inlining everything into the
// document removes the whole class of problem, and lets the host hash the one
// inline script into a strict script-src rather than naming an origin.
//
// `base: './'` because the host mounts this under a prefix that is not knowable
// here. A signed binary cannot be parameterised per install, so nothing in the
// bundle may contain an absolute path.
export default defineConfig({
  plugins: [react(), viteSingleFile()],
  base: './',
  build: {
    outDir: 'dist',
    // Emptying it would delete the .gitignore that keeps the directory
    // present in a fresh checkout, and //go:embed fails outright on a missing
    // one. The single-file build writes exactly one file, so nothing stale
    // survives anyway.
    emptyOutDir: false,
    assetsDir: 'assets',
    target: 'es2020',
    sourcemap: false,
    cssCodeSplit: false,
    assetsInlineLimit: 100_000_000,
    chunkSizeWarningLimit: 4096,
    rollupOptions: { output: { inlineDynamicImports: true } },
  },
})
