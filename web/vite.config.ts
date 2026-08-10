import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// /api 与 /api/v1/ws 反代到 Go 后端（:8080），WS 走同一 origin 免跨域。
export default defineConfig({
  plugins: [
    react(),
    // 关键：Vite 会把 react-refresh/vite-client 脚本注入 <head> 最前，把 <meta charset>
    // 推到文档 1024 字节之外——浏览器探测不到 charset 声明，Windows 中文系统按 GBK 解码
    // 导致全页面乱码。此插件在注入后把 meta charset 强制移回 <head> 第一位。
    {
      name: 'html-charset-first',
      transformIndexHtml: {
        order: 'post',
        handler(html) {
          const meta = '<meta charset="UTF-8" />'
          const cleaned = html.replace(/<meta charset=["'][^"']*["']\s*\/?>/i, '')
          return cleaned.replace('<head>', `<head>\n    ${meta}`)
        },
      },
    },
  ],
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
