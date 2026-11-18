// Хуки react-query для экранов панели.

import { QueryClient, keepPreviousData, useQuery } from '@tanstack/react-query';

import { api } from './client';
import { ApiError } from './http';

export const qk = {
  me: ['me'] as const,
  role: ['role'] as const,
  version: ['version'] as const,
  sync: ['sync'] as const,
  nodes: ['nodes'] as const,
  users: (limit: number, offset: number) => ['users', limit, offset] as const,
  usersAll: ['users'] as const,
  pairs: ['pairs'] as const,
  drift: (threshold: number) => ['drift', threshold] as const,
  applyLog: (target: string) => ['apply-log', target] as const,
};

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
  useQuery({ queryKey: qk.nodes, queryFn: api.nodes, refetchInterval: 15_000 });
export const useUsers = (limit: number, offset: number) =>
  useQuery({
    queryKey: qk.users(limit, offset),
    queryFn: () => api.users(limit, offset),
    placeholderData: keepPreviousData,
  });
export const usePairs = () =>
  useQuery({ queryKey: qk.pairs, queryFn: api.pairs, refetchInterval: 15_000 });
export const useDrift = (threshold: number) =>
  useQuery({ queryKey: qk.drift(threshold), queryFn: () => api.drift(threshold), refetchInterval: 15_000 });
export const useApplyLog = (target: string) =>
  useQuery({ queryKey: qk.applyLog(target), queryFn: () => api.applyLog(target) });

// Запись разрешена только на мастере; пока роль не известна - считаем, что нельзя.
export function useIsMaster(): boolean {
  const { data } = useRole();
  return data?.is_master ?? false;
}
