import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import 'antd/dist/reset.css';
import '@/styles/page-shell.css';

import App from './App';
import { initI18n } from './i18n';
import { makeQueryClient } from './api/queries';
import { installChunkRecovery } from './chunkRecovery';

installChunkRecovery();
initI18n();

const root = document.getElementById('root');
if (root) {
  createRoot(root).render(
    <StrictMode>
      <App queryClient={makeQueryClient()} />
    </StrictMode>,
  );
}
