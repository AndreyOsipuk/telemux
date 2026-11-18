// Заглушки для экранов «Пары» и «Дрейф», пока на мастере нет ручек pairs, drift,
// apply-log (docs/PLAN.md, раздел 2). Имена машин нейтральные, адресов нет.

import type { ApplyLogEntry, DriftResponse, Pair, Target } from './types';

const ago = (sec: number) => new Date(Date.now() - sec * 1000).toISOString();

function target(p: Partial<Target> & Pick<Target, 'code' | 'name' | 'role'>): Target {
  return {
    status: 'online',
    desired_rev: 42,
    applied_rev: 42,
    binary: { name: p.role === 'exit' ? 'mtg' : 'telemt', version: '3.5.17', canary: false },
    last_seen_at: ago(4),
    via_bridge: null,
    last_error: null,
    ...p,
  };
}

export function mockPairs(): Pair[] {
  return [
    {
      code: 'pair-a',
      name: 'A',
      relay: target({ code: 'relay-a', name: 'relay-a', role: 'relay', via_bridge: 'exit-a1' }),
      servers: [
        target({ code: 'srv-a1', name: 'srv-a1', role: 'server' }),
        target({
          code: 'srv-a2',
          name: 'srv-a2',
          role: 'server',
          binary: { name: 'telemt', version: '3.5.18+sess.1bdefc24', canary: true },
          last_seen_at: ago(12),
        }),
      ],
      exits: [
        target({ code: 'exit-a1', name: 'exit-a1', role: 'exit' }),
        target({ code: 'exit-a2', name: 'exit-a2', role: 'exit', last_seen_at: ago(31) }),
      ],
    },
    {
      code: 'pair-b',
      name: 'B',
      relay: target({
        code: 'relay-b',
        name: 'relay-b',
        role: 'relay',
        via_bridge: 'exit-b2',
        applied_rev: 41,
        status: 'degraded',
        last_error: 'конфиг отвергнут: неизвестный ключ access.user_ad_tags',
      }),
      servers: [
        target({ code: 'srv-b1', name: 'srv-b1', role: 'server', applied_rev: 41, last_seen_at: ago(95) }),
        target({
          code: 'srv-b2',
          name: 'srv-b2',
          role: 'server',
          status: 'offline',
          applied_rev: 40,
          last_seen_at: ago(3_700),
          last_error: 'нет связи с мастером',
        }),
      ],
      exits: [
        target({ code: 'exit-b1', name: 'exit-b1', role: 'exit' }),
        target({ code: 'exit-b2', name: 'exit-b2', role: 'exit' }),
      ],
    },
  ];
}

export function mockApplyLog(code: string): ApplyLogEntry[] {
  const all: Record<string, ApplyLogEntry[]> = {
    'relay-b': [
      { at: ago(600), target: 'relay-b', rev: 42, result: 'rolled_back', message: 'конфиг отвергнут: неизвестный ключ access.user_ad_tags' },
      { at: ago(7_200), target: 'relay-b', rev: 41, result: 'applied', message: null },
    ],
    'srv-b2': [
      { at: ago(3_800), target: 'srv-b2', rev: 41, result: 'failed', message: 'нет места на диске' },
      { at: ago(86_400), target: 'srv-b2', rev: 40, result: 'applied', message: null },
    ],
  };
  return (
    all[code] ?? [
      { at: ago(300), target: code, rev: 42, result: 'applied', message: null },
      { at: ago(7_300), target: code, rev: 41, result: 'applied', message: null },
    ]
  );
}

export function mockDrift(thresholdSeconds = 300): DriftResponse {
  const data = [
    { target: 'relay-b', name: 'relay-b', pair: 'pair-b', role: 'relay' as const, desired_rev: 42, applied_rev: 41, since: ago(600), last_error: 'конфиг отвергнут: неизвестный ключ access.user_ad_tags' },
    { target: 'srv-b1', name: 'srv-b1', pair: 'pair-b', role: 'server' as const, desired_rev: 42, applied_rev: 41, since: ago(420), last_error: null },
    { target: 'srv-b2', name: 'srv-b2', pair: 'pair-b', role: 'server' as const, desired_rev: 42, applied_rev: 40, since: ago(3_800), last_error: 'нет связи с мастером' },
  ];
  return {
    threshold_seconds: thresholdSeconds,
    data,
    paging: { total: data.length, limit: 20, offset: 0 },
  };
}

// Имитация сетевой задержки, чтобы были видны состояния загрузки.
export const delay = <T>(v: T, ms = 250) => new Promise<T>((res) => setTimeout(() => res(v), ms));
