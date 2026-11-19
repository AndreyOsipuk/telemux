// Источник: форк 3x-ui, frontend/src/routes.tsx (GPL-3.0). Ленивые страницы под общим
// layout; Suspense вынесен в PanelLayout.

import { lazy } from 'react';
import { Navigate, createBrowserRouter, type RouteObject } from 'react-router';

import PanelLayout from '@/layouts/PanelLayout';

const OverviewPage = lazy(() => import('@/pages/overview/OverviewPage'));
const ServersPage = lazy(() => import('@/pages/servers/ServersPage'));
const ConnectionsPage = lazy(() => import('@/pages/connections/ConnectionsPage'));
const UsersPage = lazy(() => import('@/pages/users/UsersPage'));
const MapPage = lazy(() => import('@/pages/map/MapPage'));
const NodesPage = lazy(() => import('@/pages/nodes/NodesPage'));
const EventsPage = lazy(() => import('@/pages/events/EventsPage'));
const ApiDocsPage = lazy(() => import('@/pages/api-docs/ApiDocsPage'));

export const routes: RouteObject[] = [
  {
    path: '/',
    element: <PanelLayout />,
    children: [
      { index: true, element: <OverviewPage /> },
      { path: 'servers', element: <ServersPage /> },
      { path: 'connections', element: <ConnectionsPage /> },
      { path: 'users', element: <UsersPage /> },
      { path: 'map', element: <MapPage /> },
      { path: 'nodes', element: <NodesPage /> },
      { path: 'events', element: <EventsPage /> },
      { path: 'api-docs', element: <ApiDocsPage /> },
      { path: '*', element: <Navigate to="/" replace /> },
    ],
  },
];

export const createRouter = () => createBrowserRouter(routes);
