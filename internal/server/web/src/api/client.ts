// Ручки API, которыми пользуется панель. Флот (машины, пары, дрейф, журнал
// применений) - /api/v1 по api/openapi.yaml; остальное - старые пути /api/...

import { request } from './http';
import type {
  ApplyLogEntry,
  ClusterNode,
  DriftResponse,
  JoinCommand,
  Machine,
  Me,
  NewTarget,
  NewUser,
  Page,
  Pair,
  Role,
  SyncStatus,
  User,
} from './types';

const enc = encodeURIComponent;
const paging = (limit: number, offset: number) => `limit=${limit}&offset=${offset}`;

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

  // ---- флот (/api/v1, постранично, limit до 100) ----
  machines: (limit: number, offset: number) =>
    request<Page<Machine>>(`/api/v1/machines?${paging(limit, offset)}`),
  pairs: (limit: number, offset: number) => request<Page<Pair>>(`/api/v1/pairs?${paging(limit, offset)}`),
  drift: (thresholdSeconds: number, limit: number, offset: number) =>
    request<DriftResponse>(`/api/v1/drift?threshold=${thresholdSeconds}&${paging(limit, offset)}`),
  // Добавить машину: одноразовая команда установки агента сразу в ответе (201).
  createTarget: (body: NewTarget) => request<JoinCommand>('/api/v1/targets', { method: 'POST', body }),
  // Новая одноразовая команда для существующей машины; прежняя неиспользованная сгорает.
  createJoin: (code: string) => request<JoinCommand>(`/api/v1/targets/${enc(code)}/join`, { method: 'POST' }),
  applyLog: (code: string, limit: number, offset: number) =>
    request<Page<ApplyLogEntry>>(`/api/v1/targets/${enc(code)}/apply-log?${paging(limit, offset)}`),
};
