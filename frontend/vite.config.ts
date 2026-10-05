import { fileURLToPath, URL } from 'node:url';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

/**
 * Vite build and dev configuration for the Financial Management System.
 * Provides the dev-server proxy to the Go API, route-level code splitting for
 * Reports/Export/Admin, and the Vitest wiring used by the frontend test suite.
 */
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    target: 'es2020',
    sourcemap: 'hidden',
    rollupOptions: {
      output: {
        manualChunks(id) {
          // Split lazy-loaded route modules so the entry chunk never pulls the
          // Reports, Export, or Admin routes eagerly (keeps initial LCP small).
          if (id.includes('/src/features/reports/')) {
            return 'reports';
          }
          if (id.includes('/src/features/exports/')) {
            return 'exports';
          }
          if (id.includes('/src/features/admin/')) {
            return 'admin';
          }
          return undefined;
        },
      },
    },
  },
  server: {
    proxy: {
      '/v1': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    coverage: {
      provider: 'v8',
    },
  },
});