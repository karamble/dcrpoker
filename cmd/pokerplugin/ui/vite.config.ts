import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { viteSingleFile } from 'vite-plugin-singlefile'

// One file, and relative paths inside it.
//
// Two constraints, both from where this ends up rather than from taste.
//
// Everything inlines into the one document because the document is the whole
// artifact: it is go:embedded into a binary that gets signed and then served
// from a loopback listener with no network behind it. A separate asset is a
// request that can fail; a data URI cannot. Fonts and images inline for the
// same reason, which is what `assetsInlineLimit` is doing below.
//
// `base: './'` so nothing in the bundle contains an absolute path. A signed
// binary cannot be parameterised per install.
//
// (This file used to say the page was framed at an opaque origin by dcrpulse.
// That host is gone - the game serves its own page now - but inlining is still
// right, for the reason above.)
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
