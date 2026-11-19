// Источник: форк 3x-ui, frontend/src/routes.tsx (GPL-3.0). Ленивые страницы под общим
// layout; Suspense вынесен в PanelLayout.

import { lazy } from 'react';
import { Navigate, createBrowserRouter, type RouteObject } from 'react-router';

import PanelLayout from '@/layouts/PanelLayout';

const OverviewPage = lazy(() => import('@/pages/overview/OverviewPage'));
const ServersPage = lazy(() => import('@/pages/servers/ServersPage'));
const ConnectionsPage = lazy(() => import('@/pages/connections/ConnectionsPage'));
const CertsPage = lazy(() => import('@/pages/certs/CertsPage'));
const PairsPage = lazy(() => import('@/pages/pairs/PairsPage'));
const DriftPage = lazy(() => import('@/pages/drift/DriftPage'));
const UsersPage = lazy(() => import('@/pages/users/UsersPage'));
const NodesPage = lazy(() => import('@/pages/nodes/NodesPage'));
const SyncPage = lazy(() => import('@/pages/sync/SyncPage'));
const ApiDocsPage = lazy(() => import('@/pages/api-docs/ApiDocsPage'));

export const routes: RouteObject[] = [
  {
    path: '/',
    element: <PanelLayout />,
    children: [
      { index: true, element: <OverviewPage /> },
      { path: 'servers', element: <ServersPage /> },
      { path: 'pairs', element: <PairsPage /> },
      { path: 'connections', element: <ConnectionsPage /> },
      { path: 'drift', element: <DriftPage /> },
      { path: 'users', element: <UsersPage /> },
      { path: 'nodes', element: <NodesPage /> },
      { path: 'certs', element: <CertsPage /> },
      { path: 'sync', element: <SyncPage /> },
      { path: 'api-docs', element: <ApiDocsPage /> },
      { path: '*', element: <Navigate to="/" replace /> },
    ],
  },
];

export const createRouter = () => createBrowserRouter(routes);
