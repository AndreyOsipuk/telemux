import path from 'node:path';

import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Сборка в dist/ - этот каталог вшивается в Go-бинарь через embed (см. ../web.go)
// и коммитится. В dev-режиме /api проксируется на локальный демон telemux.
const BACKEND = process.env.TELEMUX_DEV_BACKEND || 'http://localhost:8080';

// Зависимости, нужные только swagger-ui (страница /api-docs). Выносим их в отдельный
// чанк, чтобы первый экран панели не тянул несколько мегабайт ради документации API.
// Список - из vite.config.js форка 3x-ui.
const SWAGGER_ONLY = [
  'swagger-ui',
  '@babel/runtime-corejs3',
  '@scarf/scarf',
  '@swagger-api/',
  '@swaggerexpert/',
  'core-js-pure',
  'immutable',
  'js-yaml',
  'lodash',
  'highlight.js',
  'react-syntax-highlighter',
  'refractor',
  'prismjs',
  'ramda',
  'ramda-adjunct',
  'redux',
  'react-redux',
  'remarkable',
  'dompurify',
];

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { '@': path.resolve(import.meta.dirname, 'src') },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 4096,
    rolldownOptions: {
      output: {
        manualChunks(id: string) {
          if (!id.includes('node_modules')) return undefined;
          if (SWAGGER_ONLY.some((p) => id.includes(`node_modules/${p}`))) return 'swagger';
          if (/node_modules\/(antd|@ant-design|rc-|@rc-component)/.test(id)) return 'antd';
          return 'vendor';
        },
      },
    },
  },
  server: {
    proxy: {
      '/api': { target: BACKEND, changeOrigin: true },
      '/healthz': { target: BACKEND, changeOrigin: true },
    },
  },
});
