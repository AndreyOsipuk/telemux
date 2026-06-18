// Тонкий клиент к HTTP-API telemux-демона.

export interface Role { role: string; is_master: boolean }
export interface SyncStatus {
  at: string; mode: string;
  creates: number; patches: number; deletes: number;
  applied: number; failed: number; aborted: boolean; error?: string;
}
export interface Node {
  code: string; name: string; address: string;
  telemt_api_url: string; role: string; enabled: boolean; last_seen_at: string | null;
}
export interface User {
  username: string; secret: string;
  expiration_at: string | null; max_tcp_conns: number | null;
  enabled: boolean; created_at: string;
}
export interface Paging { total: number; limit: number; offset: number }
export interface NewUser { username: string; expiration_at?: string | null; max_tcp_conns?: number | null }

async function j<T>(url: string, opts?: RequestInit): Promise<T> {
  const r = await fetch(url, opts);
  if (!r.ok) {
    let msg = `${r.status}`;
    try { msg = (await r.json()).error ?? msg; } catch { /* ignore */ }
    throw new Error(msg);
  }
  return (r.status === 204 ? undefined : await r.json()) as T;
}
const post = (url: string, body?: unknown) =>
  j(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined });

export const api = {
  version: () => j<{ version: string }>('/api/version'),
  role: () => j<Role>('/api/role'),
  syncStatus: () => j<SyncStatus>('/api/sync/status'),
  syncNow: () => j<SyncStatus>('/api/sync', { method: 'POST' }),
  nodes: () => j<{ nodes: Node[] }>('/api/nodes'),

  users: (limit = 50, offset = 0) =>
    j<{ data: User[]; paging: Paging }>(`/api/users?limit=${limit}&offset=${offset}`),
  createUser: (u: NewUser) => post('/api/users', u),
  deleteUser: (username: string) => j(`/api/users/${encodeURIComponent(username)}`, { method: 'DELETE' }),
  renewUser: (username: string, expiration_at: string | null) =>
    post(`/api/users/${encodeURIComponent(username)}/renew`, { expiration_at }),
  enableUser: (username: string, on: boolean) =>
    post(`/api/users/${encodeURIComponent(username)}/${on ? 'enable' : 'disable'}`),
};
