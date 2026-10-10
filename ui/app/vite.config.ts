// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { defineConfig, loadEnv } from 'vite'
import react from '@vitejs/plugin-react'

// The console is served from the same origin as the gateway, out of the Go
// binary. Two things follow from that and are configured here.
//
// `base` stays at the root. Vite already emits hashed files into an `assets/`
// directory inside the output, and the server file-serves exactly that one
// prefix - so index.html references /assets/index-hash.js and the file is at
// dist/assets/index-hash.js. Setting base to '/assets/' as well produces
// /assets/assets/…, which 404s in a way no Go test that fetches the files
// directly will notice, and which a browser finds immediately.
//
// The dev server proxies everything that is not the console to a running
// `panorama serve`, so `npm run dev` gives hot reload against real data instead
// of a mock layer that drifts from the API.
export default defineConfig(({ mode }) => {
  // Two environments run this dev server and the gateway is not in the same
  // place in both. `make ui-dev` runs Vite on the host against `make run`, so
  // the gateway is on localhost; the compose stack runs it in a container
  // beside the gateway's, where localhost is itself. loadEnv reads the process
  // environment as well as any .env file, and is typed, so this needs no
  // @types/node.
  const env = loadEnv(mode, '.', 'PANORAMA_')
  const target = env.PANORAMA_PROXY_TARGET || 'http://localhost:8088'

  return {
    plugins: [react()],
    base: '/',
    build: {
      outDir: '../dist',
      emptyOutDir: true,
      // A source map would be the one asset in the bundle that carries the
      // source of a page holding a session. It is not shipped.
      sourcemap: false,
      // Fonts are inlined nowhere: a woff2 as a data URI defeats caching and
      // pushes the entry chunk past anything a slow connection wants.
      assetsInlineLimit: 0,
    },
    server: {
      port: 5173,
      proxy: Object.fromEntries(
        ['/api', '/auth', '/v1', '/healthz', '/readyz'].map((path) => [
          path,
          { target, changeOrigin: false },
        ]),
      ),
      // Polling is opt-in, and the compose stack opts in. File change events do
      // not cross a bind mount reliably on macOS, and hot reload that quietly
      // stops firing costs more than the polling does. On the host, where the
      // watcher works, this stays off.
      ...(env.PANORAMA_WATCH_POLL ? { watch: { usePolling: true, interval: 400 } } : {}),
    },
  }
})
