import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 开发模式下把 /api 代理到 Go 后端，避免本地跨域配置麻烦。
export default defineConfig({
  plugins: [vue()],
  server: {
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
})
