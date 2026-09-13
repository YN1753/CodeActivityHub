import { defineConfig } from 'vite';
import { fileURLToPath, URL } from 'node:url';

export default defineConfig({
  // App.js mounts onto the existing HTML template, so use Vue's compiler build.
  resolve: {
    alias: {
      vue: fileURLToPath(new URL('./node_modules/vue/dist/vue.esm-bundler.js', import.meta.url))
    }
  },
  server: {
    host: '127.0.0.1',
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:2053'
    }
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true
  }
});
