// Источник: форк 3x-ui, frontend/src/routes.tsx (GPL-3.0). Ленивые страницы под общим
// layout; Suspense вынесен в PanelLayout.

import { lazy } from 'react';
import { Navigate, createBrowserRouter, type RouteObject } from 'react-router';

import PanelLayout from '@/layouts/PanelLayout';

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
      { index: true, element: <Navigate to="/pairs" replace /> },
      { path: 'pairs', element: <PairsPage /> },
      { path: 'drift', element: <DriftPage /> },
      { path: 'users', element: <UsersPage /> },
      { path: 'nodes', element: <NodesPage /> },
      { path: 'sync', element: <SyncPage /> },
      { path: 'api-docs', element: <ApiDocsPage /> },
      { path: '*', element: <Navigate to="/pairs" replace /> },
    ],
  },
];

export const createRouter = () => createBrowserRouter(routes);
