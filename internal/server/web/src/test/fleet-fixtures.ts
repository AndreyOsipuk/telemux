// Данные флота по схемам api/openapi.yaml (Machine, PairMachine, Pair, DriftItem,
// ApplyLogEntry) и бэкенд-обработчик постраничных списков для mockFetch.
import type { ApplyLogEntry, DriftItem, Machine, Page, Pair, PairMachine } from '@/api/types';

export const NOW_ISO = () => new Date(Date.now() - 30_000).toISOString();

export function machine(code: string, over: Partial<Machine> = {}): Machine {
  return {
    code,
    kind: 'exit',
    enabled: true,
    resources: ['telemt.users'],
    agent_version: '0.6.0',
    seen_at: NOW_ISO(),
    checks: [],
    observed: [{ resource: 'telemt.users', revision: 7, status: 'applied', seen_at: NOW_ISO() }],
    ...over,
  };
}

export function pairMachine(code: string, over: Partial<PairMachine> = {}): PairMachine {
  return {
    ...machine(code),
    status: 'ok',
    desired: [{ resource: 'telemt.users', revision: 7 }],
    ...over,
  };
}

export function driftItem(target: string, over: Partial<DriftItem> = {}): DriftItem {
  return {
    target,
    kind: 'exit',
    reason: 'behind',
    resource: 'telemt.users',
    desired_revision: 8,
    observed_revision: 7,
    since: new Date(Date.now() - 3_600_000).toISOString(),
    seen_at: NOW_ISO(),
    ...over,
  };
}

export function logEntry(i: number, over: Partial<ApplyLogEntry> = {}): ApplyLogEntry {
  return {
    at: new Date(Date.UTC(2026, 10, 1, 12, 0, 0) - i * 60_000).toISOString(),
    resource: 'telemt.users',
    revision: 100 - i,
    status: 'applied',
    ...over,
  };
}

// Страница {data, paging} по limit/offset из URL.
export function pageOf<T>(all: T[], url: string): Page<T> {
  const q = new URL(url, 'http://x').searchParams;
  const limit = Number(q.get('limit') ?? 20);
  const offset = Number(q.get('offset') ?? 0);
  return { data: all.slice(offset, offset + limit), paging: { total: all.length, limit, offset } };
}

export const urlPath = (url: string) => new URL(url, 'http://x').pathname;
export const urlParam = (url: string, name: string) => new URL(url, 'http://x').searchParams.get(name);

export type { Pair };
