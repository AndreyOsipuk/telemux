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

async function j<T>(url: string, opts?: RequestInit): Promise<T> {
  const r = await fetch(url, opts);
  if (!r.ok) throw new Error(`${url}: ${r.status}`);
  return r.json() as Promise<T>;
}

export const api = {
  version: () => j<{ version: string }>('/api/version'),
  role: () => j<Role>('/api/role'),
  users: () => j<{ total: number }>('/api/users'),
  syncStatus: () => j<SyncStatus>('/api/sync/status'),
  syncNow: () => j<SyncStatus>('/api/sync', { method: 'POST' }),
  nodes: () => j<{ nodes: Node[] }>('/api/nodes'),
};
