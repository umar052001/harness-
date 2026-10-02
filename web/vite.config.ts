import { defineConfig } from 'vite'
import solid from 'vite-plugin-solid'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [solid(), tailwindcss()],
  server: {
    // Convenience for `npm run dev`: forward the API to the control plane. The
    // production path serves the built app from the same origin (cmd/server -web),
    // so this proxy exists only to keep the two processes apart during dev.
    proxy: {
      '/api': 'http://127.0.0.1:8090',
    },
  },
})