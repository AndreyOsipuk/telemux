// Тесты «Обзора»: плитки итогов, компактная таблица машин, карточка машины по клику,
// пустой флот, ошибки 401 и 500, флот больше 500 машин, сертификаты выключены.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { FLEET_REFRESH_MS, qk } from '@/api/queries';
import type { Machine } from '@/api/types';
import ru from '@/i18n/ru.json';
import OverviewPage from '@/pages/overview/OverviewPage';
import { machine, machineRuntime, overviewOf, runtimeMachine, urlPath } from './fleet-fixtures';
import { mockFetch, renderWithProviders } from './test-utils';

afterEach(() => {
  vi.unstubAllGlobals();
});

const MACHINES: Machine[] = [
  runtimeMachine('relay-a', { kind: 'relay', agent_version: '0.7.0', mtg_version: '2.1.7' }),
  runtimeMachine('exit-a1', {
    runtime_status: 'degraded',
    telemt_api_ok: false,
    host: { cpu_pct: 95, mem_pct: 80, disk_pct: 10, load1: 3.5, net_rx_mbps: 0, net_tx_mbps: 0, uptime_s: 7200 },
    connections: 3,
    users_online: 2,
  }),
  machine('exit-new', { runtime_status: 'offline', seen_at: null, agent_version: undefined }),
];

const row = (code: string) => {
  const el = within(screen.getByTestId('overview-machines'))
    .getAllByRole('row')
    .find((r) => r.getAttribute('data-row-key') === code);
  if (!el) throw new Error(`нет строки ${code}`);
  return el;
};

describe('OverviewPage', () => {
  it('плитки: машины, подключения, пары, сертификаты', async () => {
    mockFetch((url) => (url === '/api/v1/overview' ? { body: overviewOf(MACHINES) } : undefined));

    renderWithProviders(<OverviewPage />);

    const machines = await screen.findByTestId('tile-machines');
    expect(within(machines).getByText(ru.overview.machines)).toBeInTheDocument();
    expect(within(machines).getByText(ru.overview.agentsOnline).closest('.ant-statistic')).toHaveTextContent('2');
    expect(within(machines).getByText(ru.overview.agentsSilent).closest('.ant-statistic')).toHaveTextContent('1');
    expect(within(machines).getByText(ru.overview.total).closest('.ant-statistic')).toHaveTextContent('3');

    const conns = screen.getByTestId('tile-connections');
    expect(within(conns).getByText(ru.overview.total).closest('.ant-statistic')).toHaveTextContent('43');
    expect(within(conns).getByText(ru.overview.usersOnline).closest('.ant-statistic')).toHaveTextContent('9');

    const pairs = screen.getByTestId('tile-pairs');
    expect(within(pairs).getByText(ru.overview.pairsOk).closest('.ant-statistic')).toHaveTextContent('2');
    expect(within(pairs).getByText(ru.overview.pairsProblem).closest('.ant-statistic')).toHaveTextContent('1');

    const certs = screen.getByTestId('tile-certs');
    expect(within(certs).getByText('истекают за 14 дней').closest('.ant-statistic')).toHaveTextContent('1');
    expect(within(certs).getByText(ru.overview.certsMissing).closest('.ant-statistic')).toHaveTextContent('1');
  });

  it('таблица машин: статус, нагрузка, аптайм, подключения, версии', async () => {
    mockFetch((url) => (url === '/api/v1/overview' ? { body: overviewOf(MACHINES) } : undefined));

    renderWithProviders(<OverviewPage />);
    await within(screen.getByTestId('overview-machines')).findByText('relay-a');

    const relay = row('relay-a');
    expect(within(relay).getByText(ru.kinds.relay)).toBeInTheDocument();
    expect(within(relay).getByText(ru.status.online)).toBeInTheDocument();
    expect(within(relay).getByText('12.5%')).toBeInTheDocument();
    expect(within(relay).getByText('40%')).toBeInTheDocument();
    expect(within(relay).getByText('55%')).toBeInTheDocument();
    expect(within(relay).getByText('0.42')).toBeInTheDocument();
    expect(within(relay).getByText('3 д')).toBeInTheDocument();
    expect(within(relay).getByText('40')).toBeInTheDocument();
    expect(within(relay).getByText('6')).toBeInTheDocument();
    expect(within(relay).getByText('3.5.17')).toBeInTheDocument();
    expect(within(relay).getByText('2.1.7')).toBeInTheDocument();
    expect(within(relay).getByText('0.7.0')).toBeInTheDocument();

    const exit = row('exit-a1');
    expect(within(exit).getByText(ru.status.degraded)).toBeInTheDocument();
    // Нагрузка выше порога - красным.
    expect(within(exit).getByText('95%')).toHaveClass('ant-typography-danger');
    expect(within(exit).getByText('80%')).toHaveClass('ant-typography-warning');
    expect(within(exit).getByText('2 ч')).toBeInTheDocument();

    const fresh = row('exit-new');
    expect(within(fresh).getByText(ru.status.offline)).toBeInTheDocument();
  });

  it('клик по строке - карточка машины с runtime-снимком', async () => {
    const fetchFn = mockFetch((url) => {
      if (url === '/api/v1/overview') return { body: overviewOf(MACHINES) };
      if (urlPath(url) === '/api/v1/machines/relay-a/runtime') return { body: machineRuntime('relay-a') };
      return undefined;
    });
    const user = userEvent.setup();

    renderWithProviders(<OverviewPage />);
    await within(screen.getByTestId('overview-machines')).findByText('relay-a');
    await user.click(row('relay-a'));

    const state = await screen.findByTestId('machine-state');
    expect(screen.getByText('Машина relay-a')).toBeInTheDocument();
    expect(within(state).getByText('cdn.example.org')).toBeInTheDocument();
    expect(within(state).getByText('connection refused')).toBeInTheDocument();
    expect(fetchFn.mock.calls.map(([u]) => String(u))).toContain('/api/v1/machines/relay-a/runtime');
  });

  it('пустой флот', async () => {
    mockFetch((url) =>
      url === '/api/v1/overview'
        ? { body: overviewOf([], { pairs: { total: 0, ok: 0, problem: 0 }, connections: { total: 0, users_online: 0 } }) }
        : undefined,
    );

    renderWithProviders(<OverviewPage />);

    expect(await screen.findByText(ru.nodes.fleetEmpty)).toBeInTheDocument();
    expect(screen.getByTestId('tile-machines')).toBeInTheDocument();
  });

  it('сертификаты выключены - плитка так и говорит', async () => {
    mockFetch((url) => (url === '/api/v1/overview' ? { body: overviewOf(MACHINES, { certs: null }) } : undefined));

    renderWithProviders(<OverviewPage />);

    expect(await within(await screen.findByTestId('tile-certs')).findByText(ru.overview.certsOff)).toBeInTheDocument();
  });

  it('флот больше 500 машин - предупреждение', async () => {
    mockFetch((url) =>
      url === '/api/v1/overview'
        ? { body: overviewOf(MACHINES, { truncated: true, machines: { total: 812, online: 800, silent: 2 } }) }
        : undefined,
    );

    renderWithProviders(<OverviewPage />);

    expect(await screen.findByText('Показаны первые 3 из 812 машин')).toBeInTheDocument();
  });

  it('401 - «сессия истекла»', async () => {
    mockFetch((url) =>
      url === '/api/v1/overview' ? { status: 401, body: { code: 'unauthorized', message: 'unauthorized' } } : undefined,
    );

    renderWithProviders(<OverviewPage />);

    expect(await screen.findByText(ru.errors.unauthorized)).toBeInTheDocument();
  });

  it('500 - понятный текст без деталей базы', async () => {
    mockFetch((url) => (url === '/api/v1/overview' ? { status: 500, body: { code: 'internal', message: 'pq: boom' } } : undefined));

    renderWithProviders(<OverviewPage />);

    expect(await screen.findByText('Ошибка сервера (500). Подробности - в журнале демона.')).toBeInTheDocument();
    expect(screen.queryByText(/pq: boom/)).not.toBeInTheDocument();
  });

  it('обновляется раз в 30 секунд', async () => {
    mockFetch((url) => (url === '/api/v1/overview' ? { body: overviewOf(MACHINES) } : undefined));

    const { queryClient } = renderWithProviders(<OverviewPage />);
    await screen.findByTestId('tile-machines');

    const q = queryClient.getQueryCache().find({ queryKey: qk.overview });
    expect((q?.options as { refetchInterval?: unknown }).refetchInterval).toBe(FLEET_REFRESH_MS);
  });
});
