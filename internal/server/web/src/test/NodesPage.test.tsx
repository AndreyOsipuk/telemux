// Тесты экрана «Машины»: раздел «Машины флота» (/api/v1/machines, постранично) и
// узлы управления кластером (/api/nodes) со статусом по последнему heartbeat.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { FLEET_REFRESH_MS, qk } from '@/api/queries';
import type { ClusterNode, Machine } from '@/api/types';
import ru from '@/i18n/ru.json';
import NodesPage, { NODE_STALE_SECONDS, nodeStatus } from '@/pages/nodes/NodesPage';
import { machine, pageOf, urlPath } from './fleet-fixtures';
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

const MACHINES: Machine[] = [
  machine('relay-a', {
    kind: 'relay',
    via: 'exit-a1',
    agent_version: '0.6.1',
    resources: ['telemt.users', 'mtg.config'],
    observed: [
      { resource: 'telemt.users', revision: 9, status: 'applied', seen_at: new Date().toISOString() },
      { resource: 'mtg.config', revision: 2, status: 'failed', error: 'mtg: bad secret', seen_at: new Date().toISOString() },
    ],
  }),
  machine('exit-a1', {
    relay_code: 'relay-a',
    resources: [],
    observed: [],
    checks: [{ name: 'bridge', ok: false, error: 'tunnel down' }],
  }),
  machine('exit-new', { relay_code: 'relay-a', seen_at: null, agent_version: undefined, observed: [], enabled: false }),
];

function backend(machines: Machine[], nodes: ClusterNode[] = []) {
  return (url: string) => {
    if (urlPath(url) === '/api/v1/machines') return { body: pageOf(machines, url) };
    if (url === '/api/nodes') return { body: { nodes } };
    return undefined;
  };
}

const fleetRow = (code: string) => {
  const el = within(screen.getByTestId('fleet-machines'))
    .getAllByRole('row')
    .find((r) => r.getAttribute('data-row-key') === code);
  if (!el) throw new Error(`нет строки ${code}`);
  return el;
};

describe('NodesPage: машины флота', () => {
  it('код, вид, релей, агент и мост, видели, проверки, ресурсы', async () => {
    const fetchFn = mockFetch(backend(MACHINES));

    renderWithProviders(<NodesPage />);
    await within(screen.getByTestId('fleet-machines')).findByText('0.6.1');

    expect(screen.getByText(ru.nodes.fleetTitle)).toBeInTheDocument();
    const relay = fleetRow('relay-a');
    expect(within(relay).getByText(ru.kinds.relay)).toBeInTheDocument();
    expect(within(relay).getByText('0.6.1')).toBeInTheDocument();
    expect(within(relay).getByText('через exit-a1')).toBeInTheDocument();
    expect(within(relay).getByTestId('res-telemt.users')).toHaveTextContent('telemt.users 9');
    expect(within(relay).getByTestId('res-telemt.users')).toHaveAttribute('data-state', 'applied');
    expect(within(relay).getByTestId('res-mtg.config')).toHaveAttribute('data-state', 'failed');
    expect(within(relay).getByText('mtg.config: mtg: bad secret')).toBeInTheDocument();
    expect(within(relay).getByText(/^\d+ секунд\S* назад$/)).toBeInTheDocument();

    const exit = fleetRow('exit-a1');
    expect(within(exit).getByText(ru.kinds.exit)).toBeInTheDocument();
    expect(within(exit).getByText('relay-a')).toBeInTheDocument();
    expect(within(exit).getByTestId('check-bridge')).toHaveTextContent(`bridge: ${ru.fleet.checkFailed}`);
    expect(within(exit).getByText('tunnel down')).toBeInTheDocument();
    expect(within(exit).getByText(ru.fleet.noResources)).toBeInTheDocument();

    const fresh = fleetRow('exit-new');
    expect(within(fresh).getByText(ru.fleet.noAgent)).toBeInTheDocument();
    expect(within(fresh).getByText(ru.nodes.disabled)).toBeInTheDocument();

    const calls = fetchFn.mock.calls.map(([u]) => String(u)).filter((u) => u.startsWith('/api/v1/machines'));
    expect(calls[0]).toBe('/api/v1/machines?limit=20&offset=0');
  });

  it('пустой флот - «Машин флота пока нет»', async () => {
    mockFetch(backend([]));

    renderWithProviders(<NodesPage />);

    expect(await screen.findByText(ru.nodes.fleetEmpty)).toBeInTheDocument();
  });

  it('пагинация: вторая страница - offset=20', async () => {
    const many = Array.from({ length: 23 }, (_, i) => machine(`m-${String(i + 1).padStart(2, '0')}`));
    const fetchFn = mockFetch(backend(many));
    const user = userEvent.setup();

    renderWithProviders(<NodesPage />);
    const fleet = screen.getByTestId('fleet-machines');
    await within(fleet).findByText('m-01');
    expect(within(fleet).getByText('Всего 23')).toBeInTheDocument();

    await user.click(within(fleet).getByRole('button', { name: 'Страница 2' }));

    expect(await within(fleet).findByText('m-21')).toBeInTheDocument();
    expect(within(fleet).queryByText('m-01')).not.toBeInTheDocument();
    expect(fetchFn.mock.calls.map(([u]) => String(u))).toContain('/api/v1/machines?limit=20&offset=20');
  });

  it('500 у машин флота - понятный текст, реестр узлов при этом виден', async () => {
    mockFetch((url) => {
      if (urlPath(url) === '/api/v1/machines') return { status: 500, body: { code: 'internal', message: 'pq: boom' } };
      if (url === '/api/nodes') return { body: { nodes: [node('master-1', new Date().toISOString())] } };
      return undefined;
    });

    renderWithProviders(<NodesPage />);

    const fleet = screen.getByTestId('fleet-machines');
    expect(await within(fleet).findByText('Ошибка сервера (500). Подробности - в журнале демона.')).toBeInTheDocument();
    expect(screen.queryByText(/pq: boom/)).not.toBeInTheDocument();
    expect(await within(screen.getByTestId('cluster-nodes')).findByText('master-1')).toBeInTheDocument();
  });

  it('401 - «сессия истекла»', async () => {
    mockFetch((url) =>
      urlPath(url) === '/api/v1/machines' ? { status: 401, body: { code: 'unauthorized', message: 'unauthorized' } } : undefined,
    );

    renderWithProviders(<NodesPage />);

    expect(await within(screen.getByTestId('fleet-machines')).findByText(ru.errors.unauthorized)).toBeInTheDocument();
  });

  it('машины флота обновляются раз в 30 секунд', async () => {
    mockFetch(backend(MACHINES));

    const { queryClient } = renderWithProviders(<NodesPage />);
    await within(screen.getByTestId('fleet-machines')).findByText('0.6.1');

    const q = queryClient.getQueryCache().find({ queryKey: qk.machines(20, 0) });
    expect((q?.options as { refetchInterval?: unknown }).refetchInterval).toBe(FLEET_REFRESH_MS);
  });
});

describe('NodesPage: узлы управления кластером', () => {
  it('отдельная таблица с подписью, статусы и «выключена» для disabled', async () => {
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

    const cluster = screen.getByTestId('cluster-nodes');
    expect(await within(cluster).findByText('fresh')).toBeInTheDocument();
    expect(within(cluster).getByText(ru.nodes.clusterTitle)).toBeInTheDocument();
    expect(within(cluster).getByText(ru.nodes.clusterHint)).toBeInTheDocument();
    expect(within(cluster).getByText(ru.status.online)).toBeInTheDocument();
    expect(within(cluster).getByText(ru.status.offline)).toBeInTheDocument();
    expect(within(cluster).getByText(ru.status.unknown)).toBeInTheDocument();
    expect(within(cluster).getByText(ru.nodes.disabled)).toBeInTheDocument();
  });

  it('ошибка сети - баннер с понятным текстом', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))));

    renderWithProviders(<NodesPage />);

    // Оба раздела - со своим баннером.
    await screen.findAllByText(ru.errors.network, {}, { timeout: 5000 });
    expect(screen.getAllByText(ru.errors.network)).toHaveLength(2);
  });
});
