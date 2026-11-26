import { defineConfig, loadEnv } from 'vite'
import react from '@vitejs/plugin-react'
import tailwind from '@tailwindcss/vite'

export default defineConfig(({ mode }) => {
  const { GH_DEV_API } = loadEnv(mode, '.', 'GH_DEV_')
  return {
    plugins: [react(), tailwind()],
    server: {
      host: '0.0.0.0',
      proxy: { '/api': { target: GH_DEV_API || 'http://127.0.0.1:8080', ws: true } },
    },
  }
})
