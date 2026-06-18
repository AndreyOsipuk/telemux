import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Сборка в dist/ — этот каталог вшивается в Go-бинарь через embed (см. web.go).
export default defineConfig({
  plugins: [react()],
  build: { outDir: 'dist', emptyOutDir: true },
});
