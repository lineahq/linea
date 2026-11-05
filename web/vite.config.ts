import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// The build is embedded into the Go binary by internal/debugger/webui. In development
// `pnpm dev` proxies the API to `linea-server` on its default address.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/debugger/webui/static/app',
    emptyOutDir: true,
    chunkSizeWarningLimit: 6000,
    // The bundled packages' licenses, which tools/notices adds to
    // linea-server's third-party notices; the server also serves the file.
    license: { fileName: 'licenses.md' },
  },
  server: {
    proxy: { '/api': 'http://127.0.0.1:7070' },
  },
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
  },
});
