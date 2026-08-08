import { fileURLToPath } from 'node:url'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  plugins: [react()],
  resolve: {
    // Docusaurus resolves `@theme/*` through its own webpack aliases, which do
    // not exist under bare vitest — an unaliased import fails at
    // import-analysis, before any `vi.mock` factory is consulted. Point the
    // theme components our components import at local test doubles.
    alias: {
      '@theme/Tabs': fileURLToPath(new URL('./src/test/theme/Tabs.tsx', import.meta.url)),
      '@theme/TabItem': fileURLToPath(new URL('./src/test/theme/TabItem.tsx', import.meta.url)),
      '@theme/CodeBlock': fileURLToPath(new URL('./src/test/theme/CodeBlock.tsx', import.meta.url)),
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    css: false,
  },
})
