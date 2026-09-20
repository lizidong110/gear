import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

/**
 * 开发期把 /api 反代到内核，浏览器看到的始终是同源 —— 因此内核**不需要任何 CORS 头**
 * （零内核侵入是本插件的底线）。目标地址可用 GEAR_API 覆盖。
 */
const target = process.env.GEAR_API || 'http://127.0.0.1:8787'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    port: 5173,
    proxy: {
      '/api': { target, changeOrigin: true },
    },
  },
  build: {
    outDir: 'dist',
    chunkSizeWarningLimit: 1200,
  },
})
