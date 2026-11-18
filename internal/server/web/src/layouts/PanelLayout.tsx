// Оболочка страниц: сайдбар + содержимое. В 3x-ui каждая страница сама рисует
// ConfigProvider/Layout/AppSidebar, здесь это сделано один раз.

import { Suspense } from 'react';
import { Outlet } from 'react-router';
import { Layout, Spin } from 'antd';

import { useAuth } from '@/auth/AuthGate';
import { useTheme } from '@/hooks/useTheme';
import AppSidebar from './AppSidebar';
import NotMasterBanner from '@/components/NotMasterBanner';

export default function PanelLayout() {
  const { isDark } = useTheme();
  const { authEnabled, logout } = useAuth();
  return (
    <Layout className={`panel-shell${isDark ? ' is-dark' : ''}`} hasSider>
      <AppSidebar authEnabled={authEnabled} onLogout={() => void logout()} />
      <Layout className="content-shell">
        <Layout.Content className="content-area">
          <NotMasterBanner />
          <Suspense
            fallback={
              <div className="page-loading">
                <Spin size="large" />
              </div>
            }
          >
            <Outlet />
          </Suspense>
        </Layout.Content>
      </Layout>
    </Layout>
  );
}
