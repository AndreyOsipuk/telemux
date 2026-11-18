import { useMemo } from 'react';
import type { ReactNode } from 'react';
import { RouterProvider } from 'react-router/dom';
import { App as AntApp, ConfigProvider } from 'antd';
import ruRU from 'antd/locale/ru_RU';
import enUS from 'antd/locale/en_US';
import { QueryClientProvider, type QueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';

import AuthGate from '@/auth/AuthGate';
import { ThemeProvider, useTheme } from '@/hooks/useTheme';
import { createRouter } from '@/routes';

// Тема и локаль antd для всего дерева (включая экран входа).
function AntdRoot({ children }: { children: ReactNode }) {
  const { antdThemeConfig } = useTheme();
  const { i18n } = useTranslation();
  return (
    <ConfigProvider theme={antdThemeConfig} locale={i18n.language === 'en' ? enUS : ruRU}>
      <AntApp>{children}</AntApp>
    </ConfigProvider>
  );
}

export function Providers({ queryClient, children }: { queryClient: QueryClient; children: ReactNode }) {
  return (
    <ThemeProvider>
      <QueryClientProvider client={queryClient}>
        <AntdRoot>{children}</AntdRoot>
      </QueryClientProvider>
    </ThemeProvider>
  );
}

export default function App({ queryClient }: { queryClient: QueryClient }) {
  const router = useMemo(createRouter, []);
  return (
    <Providers queryClient={queryClient}>
      <AuthGate>
        <RouterProvider router={router} />
      </AuthGate>
    </Providers>
  );
}
