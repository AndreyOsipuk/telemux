// Ручки API, которыми пользуется панель. Флот (машины, пары, дрейф, журнал
// применений) - /api/v1 по api/openapi.yaml; остальное - старые пути /api/...

import { request } from './http';
import type {
  ApplyLogEntry,
  CertInfo,
  ClusterNode,
  ConnFilter,
  Connection,
  ConnectionSummary,
  LogErrorSum,
  MachineRuntime,
  Overview,
  UserConnections,
  DriftResponse,
  JoinCommand,
  ConfigSource,
  Machine,
  MachineCheck,
  MachineEvent,
  MachineKind,
  MachineMetrics,
  MetricStep,
  Me,
  NewTarget,
  NewUser,
  Page,
  Pair,
  Role,
  Server,
  ServerField,
  ServerFilter,
  ServerInput,
  ServerRenderItem,
  StackProduct,
  StackStatus,
  StackUpdateRequest,
  StackUpdateState,
  SyncStatus,
  Target,
  TargetPatch,
  User,
} from './types';

const enc = encodeURIComponent;
const paging = (limit: number, offset: number) => `limit=${limit}&offset=${offset}`;

// Строка запроса из пар имя-значение; пустые значения пропускаются.
function query(params: [string, string | number | undefined][]): string {
  const q = new URLSearchParams();
  for (const [k, v] of params) if (v !== undefined && v !== '') q.append(k, String(v));
  return q.toString();
}

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
  machinesByKind: (kind: MachineKind, limit: number, offset: number) =>
    request<Page<Machine>>(`/api/v1/machines?kind=${kind}&${paging(limit, offset)}`),
  pairs: (limit: number, offset: number) => request<Page<Pair>>(`/api/v1/pairs?${paging(limit, offset)}`),
  drift: (thresholdSeconds: number, limit: number, offset: number) =>
    request<DriftResponse>(`/api/v1/drift?threshold=${thresholdSeconds}&${paging(limit, offset)}`),
  // Добавить машину: одноразовая команда установки агента сразу в ответе (201).
  createTarget: (body: NewTarget) => request<JoinCommand>('/api/v1/targets', { method: 'POST', body }),
  // Новая одноразовая команда для существующей машины; прежняя неиспользованная сгорает.
  createJoin: (code: string) => request<JoinCommand>(`/api/v1/targets/${enc(code)}/join`, { method: 'POST' }),
  // Включить/выключить машину (и поправить настройки). Только мастер.
  updateTarget: (code: string, body: TargetPatch) =>
    request<Target>(`/api/v1/targets/${enc(code)}`, { method: 'PUT', body }),
  // Только выключенную и ни на что не ссылающуюся: иначе 409 machine_enabled / machine_in_use.
  deleteTarget: (code: string) => request<unknown>(`/api/v1/targets/${enc(code)}`, { method: 'DELETE' }),
  // Проверка связи с мастера: свежесть агента и точки входа серверов машины (до 20 с).
  checkMachine: (code: string) => request<MachineCheck>(`/api/v1/machines/${enc(code)}/check`, { method: 'POST' }),
  machineMetrics: (code: string, from: string, to: string, step: MetricStep) =>
    request<MachineMetrics>(
      `/api/v1/machines/${enc(code)}/metrics?${query([
        ['from', from],
        ['to', to],
        ['step', step],
      ])}`,
    ),
  // Версии продуктов на машинах и последние релизы GitHub (ADR 0008).
  stack: (limit: number, offset: number) => request<StackStatus>(`/api/v1/stack?${paging(limit, offset)}`),
  // Проверить релизы сейчас; чаще раза в минуту - 429.
  checkStack: () => request<{ products: StackProduct[] }>('/api/v1/stack/check', { method: 'POST' }),
  // Запуск обновления продукта на машине (202); итог - в stack (update.status).
  updateMachineProduct: (code: string, body: StackUpdateRequest) =>
    request<StackUpdateState>(`/api/v1/machines/${enc(code)}/update`, { method: 'POST', body }),
  // Тревоги после after_id по возрастанию id.
  events: (afterId: number, limit: number) =>
    request<Page<MachineEvent>>(`/api/v1/events?after_id=${afterId}&limit=${limit}`),
  applyLog: (code: string, limit: number, offset: number) =>
    request<Page<ApplyLogEntry>>(`/api/v1/targets/${enc(code)}/apply-log?${paging(limit, offset)}`),

  // ---- runtime-отчёты агентов ----
  // Обзор: итоги флота и машины с runtime-полями одним запросом (до 500 машин).
  overview: () => request<Overview>('/api/v1/overview'),
  machineRuntime: (code: string) => request<MachineRuntime>(`/api/v1/machines/${enc(code)}/runtime`),
  connections: (f: ConnFilter, limit: number, offset: number) =>
    request<Page<Connection>>(
      `/api/v1/connections?${query([
        ['machine', f.machine],
        ['product', f.product],
        ['min_connections', f.minConnections],
        ['user', f.user],
        ['limit', limit],
        ['offset', offset],
      ])}`,
    ),
  connectionsSummary: (limit: number, offset: number) =>
    request<Page<ConnectionSummary>>(`/api/v1/connections/summary?${paging(limit, offset)}`),
  // Сумма подключений по всем машинам для каждого пользователя (до 100 за запрос).
  connectionsByUser: (users: string[]) =>
    request<{ data: UserConnections[] }>(`/api/v1/connections/by-user?${query(users.map((u) => ['user', u]))}`),
  logErrors: (since: string, machine: string | undefined, limit: number, offset: number) =>
    request<Page<LogErrorSum>>(
      `/api/v1/log-errors?${query([
        ['since', since],
        ['machine', machine],
        ['limit', limit],
        ['offset', offset],
      ])}`,
    ),
  certs: (machine: string | undefined, limit: number, offset: number) =>
    request<Page<CertInfo>>(
      `/api/v1/certs?${query([
        ['machine', machine],
        ['limit', limit],
        ['offset', offset],
      ])}`,
    ),

  // ---- серверы (ADR 0005): справочник, бот пока берёт серверы из своей базы ----
  servers: (f: ServerFilter, limit: number, offset: number) =>
    request<Page<Server>>(
      `/api/v1/servers?${query([
        ['engine', f.engine],
        ['state', f.state],
        ['limit', limit],
        ['offset', offset],
      ])}`,
    ),
  serverFields: () => request<{ data: ServerField[] }>('/api/v1/servers/fields'),
  createServer: (body: ServerInput) => request<Server>('/api/v1/servers', { method: 'POST', body }),
  // Замена целиком вместе с точками входа.
  updateServer: (no: number, body: ServerInput) => request<Server>(`/api/v1/servers/${no}`, { method: 'PUT', body }),
  // Только выключенный (state=disabled), иначе 409.
  deleteServer: (no: number) => request<unknown>(`/api/v1/servers/${no}`, { method: 'DELETE' }),
  // Сравнение сборки telemux с текущими конфигами машин сервера (ADR 0006).
  serverRender: (no: number) => request<{ data: ServerRenderItem[] }>(`/api/v1/servers/${no}/render`),
  // На telemux - только при совпадении сборки, иначе 409 render_mismatch; force - всё равно.
  setConfigSource: (no: number, source: ConfigSource, force = false) =>
    request<Server>(`/api/v1/servers/${no}/config-source`, { method: 'PUT', body: { source, force } }),
};
