// Карточка пользователя (статус, срок, подключения) и «Карта» (граф из серверов:
// релей -> машина за рубежом и «напрямую», серверы с одним путём - на одной линии).
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import type { User } from '@/api/types';
import ru from '@/i18n/ru.json';
import MapPage, { DIRECT, RETIRED, buildMap, relayDomain, spread } from '@/pages/map/MapPage';
import { majorityVersion } from '@/components/MachineCells';
import UserDrawer, { userStatus } from '@/pages/users/UserDrawer';
import UsersPage from '@/pages/users/UsersPage';
import { overviewOf, runtimeMachine, urlPath } from './fleet-fixtures';
import { endpoint, server } from './server-fixtures';
import { mockFetch, renderWithProviders } from './test-utils';

afterEach(() => {
  vi.unstubAllGlobals();
});

const MASTER = { status: 200, body: { role: 'master', is_master: true } };

function user(over: Partial<User> = {}): User {
  return { username: 'ivan', secret: 's', expiration_at: null, max_tcp_conns: null, enabled: true, created_at: '2026-01-01T00:00:00Z', ...over };
}

describe('userStatus', () => {
  const now = new Date('2026-06-01T12:00:00Z');
  it.each([
    { name: 'бессрочный', u: user(), want: 'active' },
    { name: 'срок впереди', u: user({ expiration_at: '2026-06-10T00:00:00Z' }), want: 'active' },
    { name: 'срок прошёл', u: user({ expiration_at: '2026-05-01T00:00:00Z' }), want: 'expired' },
    { name: 'выключен важнее срока', u: user({ enabled: false, expiration_at: '2026-05-01T00:00:00Z' }), want: 'disabled' },
  ])('$name -> $want', ({ u, want }) => {
    expect(userStatus(u, now)).toBe(want);
  });
});

describe('UserDrawer', () => {
  it('продление на 90 дней, бессрочно и своя дата - через renew', async () => {
    const ui = userEvent.setup();
    const renews: unknown[] = [];
    mockFetch((url, init) => {
      if (url === '/api/role') return MASTER;
      if (urlPath(url) === '/api/users/ivan/renew') {
        renews.push(JSON.parse(String(init?.body)));
        return { body: { ok: true } };
      }
      if (urlPath(url) === '/api/v1/connections') return { body: { data: [], paging: { total: 0, limit: 20, offset: 0 } } };
      return undefined;
    });
    const exp = new Date(Date.now() + 10 * 86400_000).toISOString();
    renderWithProviders(<UserDrawer user={user({ expiration_at: exp })} onClose={() => {}} />);
    const drawer = await screen.findByTestId('user-drawer');
    expect(within(drawer).getByText(ru.users.status.active)).toBeInTheDocument();

    const plus90 = within(drawer).getByRole('button', { name: '+90 дней' });
    await waitFor(() => expect(plus90).toBeEnabled());
    await ui.click(plus90);
    await waitFor(() => expect(renews).toHaveLength(1));
    // От текущего срока, а не от сегодня.
    const got = Date.parse((renews[0] as { expiration_at: string }).expiration_at);
    expect(got).toBe(Date.parse(exp) + 90 * 86400_000);

    await ui.click(within(drawer).getByRole('button', { name: ru.users.makeUnlimited }));
    await waitFor(() => expect(renews).toHaveLength(2));
    expect(renews[1]).toEqual({ expiration_at: null });
  });

  it('истёкший пользователь - статус и «истёк»', async () => {
    mockFetch((url) => (url === '/api/role' ? MASTER : urlPath(url) === '/api/v1/connections' ? { body: { data: [], paging: { total: 0, limit: 20, offset: 0 } } } : undefined));
    renderWithProviders(<UserDrawer user={user({ expiration_at: '2026-01-05T00:00:00Z' })} onClose={() => {}} />);
    const drawer = await screen.findByTestId('user-drawer');
    expect(within(drawer).getByText(ru.users.status.expired)).toBeInTheDocument();
    expect(within(drawer).getByText('истёк 05.01.2026')).toBeInTheDocument();
  });
});

describe('UsersPage: карточка вместо «+30»', () => {
  it('кнопки «+30» нет; клик по строке открывает карточку', async () => {
    const ui = userEvent.setup();
    mockFetch((url) => {
      if (url === '/api/role') return MASTER;
      if (url.startsWith('/api/users')) return { body: { data: [user()], paging: { total: 1, limit: 20, offset: 0 } } };
      if (urlPath(url) === '/api/v1/connections') return { body: { data: [], paging: { total: 0, limit: 20, offset: 0 } } };
      return undefined;
    });
    renderWithProviders(<UsersPage />);
    const name = await screen.findByText('ivan');
    const tr = name.closest('tr')!;
    expect(within(tr).queryByRole('button', { name: /\+30/ })).not.toBeInTheDocument();
    await ui.click(name);
    expect(await screen.findByTestId('user-drawer')).toBeInTheDocument();
  });
});

describe('buildMap', () => {
  it('рёбра из серверов: одна машина за разными релеями, прямые - от «напрямую»', () => {
    const s1 = server(1, {
      relay_code: 'pr1',
      node_code: 'ps1',
      endpoints: [endpoint('ee'), endpoint('dd', { via_relay: false, enabled: false })],
    });
    const s6 = server(6, { engine: 'mtg', relay_code: 'pr2', node_code: 'ps1', endpoints: [endpoint('dd')] });
    const s5 = server(5, { engine: 'mtg', relay_code: 'pr1', node_code: 'ps5', endpoints: [endpoint('dd'), endpoint('ee')] });
    const m = buildMap([s6, s1, s5]);
    expect(m.relays).toEqual(['pr1', 'pr2', DIRECT]);
    // ps1 - за pr1 и pr2 (в среднем ниже), ps5 - только за pr1: ps5 выше.
    expect(m.exits).toEqual(['ps5', 'ps1']);
    const edge = (from: string, to: string) => m.edges.find((e) => e.from === from && e.to === to);
    expect(edge('pr1', 'ps1')?.servers.map((s) => [s.server.no, s.enabled])).toEqual([[1, true]]);
    expect(edge('pr2', 'ps1')?.servers.map((s) => s.server.no)).toEqual([6]);
    expect(edge(DIRECT, 'ps1')?.servers.map((s) => [s.server.no, s.enabled])).toEqual([[1, false]]);
    // Две точки сервера 5 через один релей - одно ребро, сервер один раз.
    expect(edge('pr1', 'ps5')?.servers).toHaveLength(1);
  });

  it('выключенный сервер - путь выключен; без машины - не на карте', () => {
    const m = buildMap([
      server(2, { state: 'disabled', relay_code: 'pr1', node_code: 'ps2', endpoints: [endpoint('ee')] }),
      server(3, { relay_code: 'pr1', node_code: null, endpoints: [endpoint('ee')] }),
    ]);
    expect(m.edges).toHaveLength(1);
    expect(m.edges[0].servers[0].enabled).toBe(false);
  });

  it('выведенная машина: ребро только до релея (в «выход выведен»), прямых нет', () => {
    const m = buildMap([
      server(4, {
        relay_code: 'pr4',
        node_code: 'ps4',
        node_retired: true,
        endpoints: [endpoint('ee'), endpoint('dd', { via_relay: false })],
      }),
    ]);
    expect(m.exits).toEqual([RETIRED]);
    expect(m.relays).toEqual(['pr4']);
    expect(m.edges.map((e) => `${e.from}->${e.to}`)).toEqual([`pr4->${RETIRED}`]);
  });

  it('серверы на релее и домен релея в ссылках', () => {
    const s1 = server(1, { relay_code: 'pr1', node_code: 'ps1', endpoints: [endpoint('ee', { host: 'pr1-ee.example.org' })] });
    const s5 = server(5, { engine: 'mtg', relay_code: 'pr1', node_code: 'ps5', endpoints: [endpoint('dd', { host: 'pr5.example.org' })] });
    expect(buildMap([s5, s1]).relayServers.pr1.map((s) => s.no)).toEqual([1, 5]);
    expect(relayDomain(s1)).toBe('pr1');
    expect(relayDomain(s5)).toBe('pr5');
    expect(relayDomain(server(9, { endpoints: [endpoint('ee', { host: '192.0.2.1' })] }))).toBeUndefined();
  });

  it('majorityVersion - самая частая версия', () => {
    expect(majorityVersion(['3.5.17', '3.5.17', '3.5.16', null, undefined])).toBe('3.5.17');
    expect(majorityVersion([null, ''])).toBeUndefined();
  });

  it('сортировка: машины за рубежом напротив своих релеев, затем по номеру сервера', () => {
    const mk = (no: number, relay: string, node: string) => server(no, { relay_code: relay, node_code: node, endpoints: [endpoint('ee')] });
    const m = buildMap([mk(1, 'pr1', 'ps9'), mk(2, 'pr2', 'ps1'), mk(5, 'pr1', 'ps5'), mk(4, 'pr4', 'ps2')]);
    expect(m.relays).toEqual(['pr1', 'pr2', 'pr4']);
    expect(m.exits).toEqual(['ps9', 'ps5', 'ps1', 'ps2']);
  });

  it('spread разводит подписи без наложения', () => {
    expect(spread([100, 105, 50], 28)).toEqual([100, 128, 50]);
  });
});

describe('MapPage', () => {
  it('машины и подписи серверов; машина без серверов - отдельно', async () => {
    mockFetch((url) => {
      if (urlPath(url) === '/api/v1/servers')
        return {
          body: {
            data: [server(1, { relay_code: 'pr1', node_code: 'ps1', endpoints: [endpoint('ee')] })],
            paging: { total: 1, limit: 100, offset: 0 },
          },
        };
      if (url === '/api/v1/overview')
        return { body: overviewOf([runtimeMachine('pr1', { kind: 'relay' }), runtimeMachine('ps1'), runtimeMachine('vega')]) };
      return undefined;
    });
    renderWithProviders(<MapPage />);
    expect(await screen.findByTestId('map-node-pr1')).toBeInTheDocument();
    expect(screen.getByTestId('map-node-ps1')).toBeInTheDocument();
    expect(within(screen.getByTestId('map-edge-pr1-ps1')).getByText(/1 · telemt/)).toBeInTheDocument();
    expect(screen.getByTestId('map-relay-servers-pr1')).toHaveTextContent('telemt 1');
    expect(await screen.findByText('vega')).toBeInTheDocument();
    expect(screen.getByText(ru.map.unplaced)).toBeInTheDocument();
  });
});
