// Хуки react-query для экранов панели.

import { QueryClient, keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { api } from './client';
import { ApiError } from './http';
import type { NewTarget, PairMachine } from './types';

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
};

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
    predicate: (q) => q.queryKey[0] === 'machines' || q.queryKey[0] === 'pairs' || q.queryKey[0] === 'drift',
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
