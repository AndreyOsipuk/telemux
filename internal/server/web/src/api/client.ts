// Ручки API, которыми пользуется панель. Пути - текущие (/api/...). Переход на
// /api/v1 и сгенерированный клиент - вместе с api/openapi.yaml.

import { request } from './http';
import { delay, mockApplyLog, mockDrift, mockPairs } from './mocks';
import type {
  ApplyLogEntry,
  ClusterNode,
  DriftResponse,
  Me,
  NewUser,
  Page,
  Pair,
  Role,
  SyncStatus,
  User,
} from './types';

const enc = encodeURIComponent;

// Экраны на заглушках: ручек pairs/drift/apply-log на мастере ещё нет.
export const USES_MOCKS = { pairs: true, drift: true } as const;

export const api = {
  me: () => request<Me>('/api/me'),
  login: (username: string, password: string) =>
    request<{ ok: boolean }>('/api/login', {
      method: 'POST',
      body: { username, password },
      skipAuthRedirect: true,
    }),
  logout: () => request<{ ok: boolean }>('/api/logout', { method: 'POST', skipAuthRedirect: true }),

  version: () => request<{ version: string }>('/api/version'),
  role: () => request<Role>('/api/role'),
  syncStatus: () => request<SyncStatus>('/api/sync/status'),
  syncNow: () => request<SyncStatus>('/api/sync', { method: 'POST' }),
  nodes: () => request<{ nodes: ClusterNode[] }>('/api/nodes'),

  users: (limit: number, offset: number) =>
    request<Page<User>>(`/api/users?limit=${limit}&offset=${offset}`),
  createUser: (u: NewUser) => request<User>('/api/users', { method: 'POST', body: u }),
  deleteUser: (username: string) =>
    request<{ ok: boolean }>(`/api/users/${enc(username)}`, { method: 'DELETE' }),
  renewUser: (username: string, expiration_at: string | null) =>
    request<{ ok: boolean }>(`/api/users/${enc(username)}/renew`, {
      method: 'POST',
      body: { expiration_at },
    }),
  setUserEnabled: (username: string, on: boolean) =>
    request<{ ok: boolean }>(`/api/users/${enc(username)}/${on ? 'enable' : 'disable'}`, {
      method: 'POST',
    }),

  // ---- заглушки (USES_MOCKS) ----
  pairs: (): Promise<Pair[]> => delay(mockPairs()),
  drift: (thresholdSeconds?: number): Promise<DriftResponse> => delay(mockDrift(thresholdSeconds)),
  applyLog: (target: string): Promise<ApplyLogEntry[]> => delay(mockApplyLog(target)),
  applyTarget: (target: string): Promise<{ target: string }> => delay({ target }, 400),
  retryTarget: (target: string): Promise<{ target: string }> => delay({ target }, 400),
};
