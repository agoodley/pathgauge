import { defineConfig } from 'vite';

// The Go server embeds internal/server/ui/dist (see internal/server/metrics.go).
// During development, `npm run dev` proxies the API and the live WebSocket to
// a running PathGauge server (default: the scripts/dev-stack.sh instance).
const target = process.env.PATHGAUGE_URL ?? 'https://127.0.0.1:17443';

export default defineConfig({
  base: '/',
  esbuild: { jsx: 'automatic' },
  build: {
    outDir: '../internal/server/ui/dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 1200,
    rollupOptions: {
      output: {
        manualChunks: { echarts: ['echarts/core', 'echarts/charts', 'echarts/components', 'echarts/renderers'] },
      },
    },
  },
  server: {
    proxy: {
      '/api': {
        target,
        secure: false,
        changeOrigin: true,
        ws: true,
        // The server's WebSocket accept checks Origin against Host.
        configure: (proxy) => {
          proxy.on('proxyReqWs', (req) => req.setHeader('origin', target));
        },
      },
      '/share': { target, secure: false, changeOrigin: true },
    },
  },
});
