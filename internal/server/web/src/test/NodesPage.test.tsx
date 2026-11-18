// Тесты экрана «Машины»: статус по последнему heartbeat.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';

import type { ClusterNode } from '@/api/types';
import ru from '@/i18n/ru.json';
import NodesPage, { NODE_STALE_SECONDS, nodeStatus } from '@/pages/nodes/NodesPage';
import { mockFetch, renderWithProviders } from './test-utils';

function node(code: string, last_seen_at: string | null, enabled = true): ClusterNode {
  return {
    code,
    name: code,
    address: 'srv-1.example.net',
    telemt_api_url: '',
    role: 'replica',
    enabled,
    last_seen_at,
  };
}

const NOW = Date.parse('2026-06-01T12:00:00Z');
const ago = (sec: number) => new Date(NOW - sec * 1000).toISOString();

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('nodeStatus', () => {
  it.each([
    { name: 'никогда не видели (null)', seen: null, want: 'unknown' },
    { name: 'битая дата', seen: 'not-a-date', want: 'unknown' },
    { name: 'только что', seen: ago(0), want: 'online' },
    { name: 'свежий heartbeat', seen: ago(10), want: 'online' },
    { name: 'ровно на пороге', seen: ago(NODE_STALE_SECONDS), want: 'online' },
    { name: 'старше порога на секунду', seen: ago(NODE_STALE_SECONDS + 1), want: 'offline' },
    { name: 'час молчания', seen: ago(3600), want: 'offline' },
    { name: 'время в будущем (сбитые часы)', seen: ago(-120), want: 'online' },
  ])('$name -> $want', ({ seen, want }) => {
    expect(nodeStatus(node('n', seen), NOW)).toBe(want);
  });
});

describe('NodesPage', () => {
  it('статусы машин в таблице и «выключена» для disabled', async () => {
    const now = Date.now();
    const iso = (sec: number) => new Date(now - sec * 1000).toISOString();
    mockFetch((url) =>
      url === '/api/nodes'
        ? {
            body: {
              nodes: [
                node('fresh', iso(5)),
                node('stale', iso(NODE_STALE_SECONDS * 10)),
                node('never', null),
                node('off', iso(5), false),
              ],
            },
          }
        : undefined,
    );

    renderWithProviders(<NodesPage />);

    expect(await screen.findByText('fresh')).toBeInTheDocument();
    expect(screen.getByText(ru.status.online)).toBeInTheDocument();
    expect(screen.getByText(ru.status.offline)).toBeInTheDocument();
    expect(screen.getByText(ru.status.unknown)).toBeInTheDocument();
    expect(screen.getByText(ru.nodes.disabled)).toBeInTheDocument();
  });

  it('ошибка сети - баннер с понятным текстом', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))));

    renderWithProviders(<NodesPage />);

    expect(await screen.findByText(ru.errors.network, {}, { timeout: 5000 })).toBeInTheDocument();
  });
});
