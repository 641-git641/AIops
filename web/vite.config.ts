import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// /api 与 /api/v1/ws 反代到 Go 后端（:8080），WS 走同一 origin 免跨域。
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
        ws: true,
      },
    },
  },
})
