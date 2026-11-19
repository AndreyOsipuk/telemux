// Хуки react-query для экранов панели.

import { QueryClient, keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { api } from './client';
import { machineUsage } from '@/utils/machineUsage';
import { useMemo } from 'react';
import { ApiError } from './http';
import type { StackStatus, StackUpdateRequest, ConfigSource, ConnFilter, MachineEvent, MachineKind, MetricStep, NewTarget, PairMachine, ServerFilter, ServerInput } from './types';

export const qk = {
  me: ['me'] as const,
  role: ['role'] as const,
  version: ['version'] as const,
  sync: ['sync'] as const,
  nodes: ['nodes'] as const,
  users: (limit: number, offset: number) => ['users', limit, offset] as const,
  usersAll: ['users'] as const,
  machines: (limit: number, offset: number) => ['machines', limit, offset] as const,
  pairs: (limit: number, offset: number) => ['pairs', limit, offset] as const,
  // Под префиксом pairs: инвалидация пар обновляет и список релеев.
  relays: ['pairs', 'relays'] as const,
  drift: (threshold: number, limit: number, offset: number) => ['drift', threshold, limit, offset] as const,
  applyLog: (code: string, limit: number, offset: number) => ['apply-log', code, limit, offset] as const,
  overview: ['overview'] as const,
  machineRuntime: (code: string) => ['machine-runtime', code] as const,
  connections: (f: ConnFilter, limit: number, offset: number) => ['connections', f, limit, offset] as const,
  connectionsSummary: (limit: number, offset: number) => ['connections', 'summary', limit, offset] as const,
  connectionsByUser: (users: string[]) => ['connections', 'by-user', users] as const,
  logErrors: (machine: string | undefined, limit: number, offset: number) =>
    ['log-errors', machine ?? '', limit, offset] as const,
  certs: (machine: string | undefined, limit: number, offset: number) => ['certs', machine ?? '', limit, offset] as const,
  servers: (f: ServerFilter, limit: number, offset: number) => ['servers', f, limit, offset] as const,
  serversAll: ['servers'] as const,
  // Отдельный префикс: запись сервера не должна перечитывать каталог полей.
  serverFields: ['server-fields'] as const,
  // Под префиксом machines: инвалидация флота обновляет и выбор машин в форме сервера.
  machinesByKind: (kind: MachineKind) => ['machines', 'kind', kind] as const,
  machineMetrics: (code: string, range: MetricRange) => ['machine-metrics', code, range] as const,
  events: ['events'] as const,
  stack: (limit: number, offset: number) => ['stack', limit, offset] as const,
  stackAll: ['stack'] as const,
  // Под префиксом servers: смена источника и правка сервера перечитывают сборку.
  serverRender: (no: number) => ['servers', 'render', no] as const,
};

// Подключения меняются быстро: экран «Подключения» обновляется раз в 15 секунд.
export const CONNECTIONS_REFRESH_MS = 15_000;

// Экраны флота (Пары, Дрейф, Машины) обновляются сами раз в 30 секунд.
export const FLEET_REFRESH_MS = 30_000;

// Повторяем только сбои сети и 5xx: 4xx от повтора не исправится.
function shouldRetry(count: number, err: unknown): boolean {
  if (err instanceof ApiError && err.status >= 400 && err.status < 500) return false;
  return count < 1;
}

export function makeQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: { staleTime: 10_000, refetchOnWindowFocus: true, retry: shouldRetry },
      mutations: { retry: 0 },
    },
  });
}

export const useRole = () => useQuery({ queryKey: qk.role, queryFn: api.role, staleTime: 30_000 });
export const useVersion = () =>
  useQuery({ queryKey: qk.version, queryFn: api.version, staleTime: Infinity });
export const useSyncStatus = () =>
  useQuery({ queryKey: qk.sync, queryFn: api.syncStatus, refetchInterval: 15_000 });
export const useNodes = () =>
  useQuery({ queryKey: qk.nodes, queryFn: api.nodes, refetchInterval: FLEET_REFRESH_MS });
export const useUsers = (limit: number, offset: number) =>
  useQuery({
    queryKey: qk.users(limit, offset),
    queryFn: () => api.users(limit, offset),
    placeholderData: keepPreviousData,
  });
export const useMachines = (limit: number, offset: number) =>
  useQuery({
    queryKey: qk.machines(limit, offset),
    queryFn: () => api.machines(limit, offset),
    placeholderData: keepPreviousData,
    refetchInterval: FLEET_REFRESH_MS,
  });
export const usePairs = (limit: number, offset: number) =>
  useQuery({
    queryKey: qk.pairs(limit, offset),
    queryFn: () => api.pairs(limit, offset),
    placeholderData: keepPreviousData,
    refetchInterval: FLEET_REFRESH_MS,
  });
export const useDrift = (threshold: number, limit: number, offset: number) =>
  useQuery({
    queryKey: qk.drift(threshold, limit, offset),
    queryFn: () => api.drift(threshold, limit, offset),
    placeholderData: keepPreviousData,
    refetchInterval: FLEET_REFRESH_MS,
  });
export const useApplyLog = (code: string, limit: number, offset: number) =>
  useQuery({
    queryKey: qk.applyLog(code, limit, offset),
    queryFn: () => api.applyLog(code, limit, offset),
    placeholderData: keepPreviousData,
    refetchInterval: FLEET_REFRESH_MS,
  });

export const useOverview = () =>
  useQuery({ queryKey: qk.overview, queryFn: api.overview, refetchInterval: FLEET_REFRESH_MS });
export const useMachineRuntime = (code: string) =>
  useQuery({ queryKey: qk.machineRuntime(code), queryFn: () => api.machineRuntime(code), refetchInterval: FLEET_REFRESH_MS });
export const useConnections = (f: ConnFilter, limit: number, offset: number) =>
  useQuery({
    queryKey: qk.connections(f, limit, offset),
    queryFn: () => api.connections(f, limit, offset),
    placeholderData: keepPreviousData,
    refetchInterval: CONNECTIONS_REFRESH_MS,
  });
export const useConnectionsSummary = (limit: number, offset: number) =>
  useQuery({
    queryKey: qk.connectionsSummary(limit, offset),
    queryFn: () => api.connectionsSummary(limit, offset),
    placeholderData: keepPreviousData,
    refetchInterval: CONNECTIONS_REFRESH_MS,
  });
// Пустой список - запроса нет (ручка требует хотя бы одного пользователя).
export const useConnectionsByUser = (users: string[]) =>
  useQuery({
    queryKey: qk.connectionsByUser(users),
    queryFn: () => api.connectionsByUser(users),
    enabled: users.length > 0,
    placeholderData: keepPreviousData,
    refetchInterval: CONNECTIONS_REFRESH_MS,
  });
// Окно журнала на мастере - 24 часа; since считается при каждом запросе, иначе
// через минуту после открытия экрана автообновление получило бы 400 (since старше окна).
export const LOG_WINDOW_MS = 24 * 3600 * 1000;
export const logSince = (now = Date.now()) => new Date(now - LOG_WINDOW_MS).toISOString().replace(/\.\d{3}Z$/, 'Z');
export const useLogErrors = (machine: string | undefined, limit: number, offset: number) =>
  useQuery({
    queryKey: qk.logErrors(machine, limit, offset),
    queryFn: () => api.logErrors(logSince(), machine, limit, offset),
    placeholderData: keepPreviousData,
    refetchInterval: FLEET_REFRESH_MS,
  });
export const useCerts = (machine: string | undefined, limit: number, offset: number) =>
  useQuery({
    queryKey: qk.certs(machine, limit, offset),
    queryFn: () => api.certs(machine, limit, offset),
    placeholderData: keepPreviousData,
    refetchInterval: FLEET_REFRESH_MS,
  });

// Все релеи флота - для выбора релея нового выхода. /api/v1/pairs отдаёт
// постранично по релеям, так что страница в 100 пар - это 100 релеев.
const RELAYS_PAGE = 100;
const RELAYS_MAX_PAGES = 20;

export async function fetchAllRelays(): Promise<PairMachine[]> {
  const out: PairMachine[] = [];
  for (let page = 0; page < RELAYS_MAX_PAGES; page++) {
    const offset = page * RELAYS_PAGE;
    const res = await api.pairs(RELAYS_PAGE, offset);
    for (const p of res.data) if (p.relay) out.push(p.relay);
    if (res.data.length === 0 || offset + res.data.length >= res.paging.total) break;
  }
  return out;
}

export const useRelays = (enabled = true) =>
  useQuery({ queryKey: qk.relays, queryFn: fetchAllRelays, enabled });

// Машины и пары после добавления машины - чтобы она сразу появилась в списках.
export function invalidateFleet(qc: QueryClient) {
  return qc.invalidateQueries({
    predicate: (q) =>
      q.queryKey[0] === 'machines' || q.queryKey[0] === 'pairs' || q.queryKey[0] === 'drift' || q.queryKey[0] === 'overview',
  });
}

export function useCreateTarget() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: NewTarget) => api.createTarget(body),
    onSuccess: () => void invalidateFleet(qc),
  });
}

export function useCreateJoin() {
  return useMutation({ mutationFn: (code: string) => api.createJoin(code) });
}

// Запись разрешена только на мастере; пока роль не известна - считаем, что нельзя.
export function useIsMaster(): boolean {
  const { data } = useRole();
  return data?.is_master ?? false;
}

// ---- серверы (ADR 0005) ----

export const useServers = (f: ServerFilter, limit: number, offset: number) =>
  useQuery({
    queryKey: qk.servers(f, limit, offset),
    queryFn: () => api.servers(f, limit, offset),
    placeholderData: keepPreviousData,
    refetchInterval: FLEET_REFRESH_MS,
  });

// Каталог полей меняется только с версией мастера.
export const useServerFields = (enabled = true) =>
  useQuery({ queryKey: qk.serverFields, queryFn: api.serverFields, staleTime: Infinity, enabled });

// Машины одного вида для выбора релея и машины за рубежом: первой сотни хватает флоту.
export const MACHINES_PICK_LIMIT = 100;
export const useMachinesByKind = (kind: MachineKind, enabled = true) =>
  useQuery({
    queryKey: qk.machinesByKind(kind),
    queryFn: () => api.machinesByKind(kind, MACHINES_PICK_LIMIT, 0),
    enabled,
  });

// Создание (no === undefined) или замена сервера целиком.
export function useSaveServer() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ no, body }: { no?: number; body: ServerInput }) =>
      no === undefined ? api.createServer(body) : api.updateServer(no, body),
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.serversAll }),
  });
}

export function useDeleteServer() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (no: number) => api.deleteServer(no),
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.serversAll }),
  });
}

// ---- машины как ноды 3x-ui (ADR 0007) ----

// Включить или выключить машину: сразу обновляются флот, пары и серверы.
export function useSetTargetEnabled() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ code, enabled }: { code: string; enabled: boolean }) => api.updateTarget(code, { enabled }),
    onSuccess: () => void invalidateFleet(qc),
  });
}

export function useDeleteTarget() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (code: string) => api.deleteTarget(code),
    onSuccess: () => void invalidateFleet(qc),
  });
}

// Проверка связи - действие по кнопке (до 20 с), результат не кешируется.
export function useCheckMachine() {
  return useMutation({ mutationFn: (code: string) => api.checkMachine(code) });
}

// Окна графиков нагрузки и шаг: точек не больше 2000 (лимит ручки) и не меньше ~24.
export type MetricRange = '1h' | '6h' | '24h' | '7d';
export const METRIC_RANGES: Record<MetricRange, { ms: number; step: MetricStep }> = {
  '1h': { ms: 3600_000, step: '1m' },
  '6h': { ms: 6 * 3600_000, step: '1m' },
  '24h': { ms: 24 * 3600_000, step: '5m' },
  '7d': { ms: 7 * 24 * 3600_000, step: '1h' },
};

// Время для параметров ручки: без миллисекунд, UTC.
const isoSec = (ms: number) => new Date(ms).toISOString().replace(/\.\d{3}Z$/, 'Z');

export const useMachineMetrics = (code: string, range: MetricRange) =>
  useQuery({
    queryKey: qk.machineMetrics(code, range),
    queryFn: () => {
      const now = Date.now();
      const r = METRIC_RANGES[range];
      // Окно [from, to): to на минуту вперёд, чтобы попала текущая неполная минута.
      return api.machineMetrics(code, isoSec(now - r.ms), isoSec(now + 60_000), r.step);
    },
    placeholderData: keepPreviousData,
    refetchInterval: 60_000,
  });

// Лента тревог: ручка отдаёт события по возрастанию id после after_id (хранение 30 дней),
// панель забирает всё постранично и показывает новые первыми.
export const EVENTS_PAGE = 500;
export const EVENTS_MAX_PAGES = 20;

export async function fetchAllEvents(): Promise<{ data: MachineEvent[]; truncated: boolean }> {
  const out: MachineEvent[] = [];
  let after = 0;
  for (let page = 0; page < EVENTS_MAX_PAGES; page++) {
    const res = await api.events(after, EVENTS_PAGE);
    out.push(...res.data);
    if (res.data.length === 0 || res.data.length >= res.paging.total) return { data: out.reverse(), truncated: false };
    after = res.data[res.data.length - 1].id;
  }
  return { data: out.reverse(), truncated: true };
}

export const useEvents = () =>
  useQuery({ queryKey: qk.events, queryFn: fetchAllEvents, refetchInterval: FLEET_REFRESH_MS });

// ---- сборка конфигов (ADR 0006) ----

export const useServerRender = (no: number, enabled = true) =>
  useQuery({ queryKey: qk.serverRender(no), queryFn: () => api.serverRender(no), enabled, refetchInterval: FLEET_REFRESH_MS });

export function useSetConfigSource() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ no, source, force }: { no: number; source: ConfigSource; force?: boolean }) =>
      api.setConfigSource(no, source, force),
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.serversAll }),
  });
}

// ---- версии и обновления (ADR 0008) ----

// Пока хоть одно обновление идёт - опрос раз в 5 с, иначе раз в минуту.
export const STACK_PENDING_REFRESH_MS = 5_000;
export const STACK_REFRESH_MS = 60_000;

export function hasPendingUpdate(s: StackStatus | undefined): boolean {
  return (s?.data ?? []).some((m) => m.telemt?.update?.status === 'pending' || m.mtg?.update?.status === 'pending');
}

export const useStack = (limit: number, offset: number) =>
  useQuery({
    queryKey: qk.stack(limit, offset),
    queryFn: () => api.stack(limit, offset),
    placeholderData: keepPreviousData,
    refetchInterval: (q) => (hasPendingUpdate(q.state.data) ? STACK_PENDING_REFRESH_MS : STACK_REFRESH_MS),
  });

export function useCheckStack() {
  const qc = useQueryClient();
  return useMutation({ mutationFn: api.checkStack, onSuccess: () => void qc.invalidateQueries({ queryKey: qk.stackAll }) });
}

export function useUpdateMachineProduct() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ code, body }: { code: string; body: StackUpdateRequest }) => api.updateMachineProduct(code, body),
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.stackAll }),
  });
}

// Роль каждой машины по серверам (выход, выведена, не в серверах) - для серых счётчиков.
export function useMachineUsage() {
  const servers = useServers({}, 100, 0);
  return useMemo(() => machineUsage(servers.data?.data ?? []), [servers.data]);
}
