import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// 开发时把 /api 代理到 Go 后端（configs/config.yaml 默认监听 :8000）
export default defineConfig({
  plugins: [react()],
  // 相对路径打包，dist 可直接用静态服务器/本地预览打开
  base: './',
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: process.env.VITE_PROXY_TARGET || 'http://localhost:8000',
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
  },
});
