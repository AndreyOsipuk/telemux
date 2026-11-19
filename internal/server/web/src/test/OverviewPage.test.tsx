// Тесты «Обзора»: плитки итогов, список проблем, карточка машины по клику,
// пустой флот, ошибки 401 и 500, флот больше 500 машин, сертификаты выключены.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { FLEET_REFRESH_MS, qk } from '@/api/queries';
import type { Machine } from '@/api/types';
import ru from '@/i18n/ru.json';
import OverviewPage, { fleetProblems } from '@/pages/overview/OverviewPage';
import { NOW_ISO, machine, machineRuntime, overviewOf, runtimeMachine, urlPath } from './fleet-fixtures';
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

const problemRow = (code: string, kind: string) => {
  const el = within(screen.getByTestId('overview-problems'))
    .getAllByRole('row')
    .find((r) => r.getAttribute('data-row-key')?.startsWith(`${code}/${kind}/`));
  if (!el) throw new Error(`нет проблемы ${code}/${kind}`);
  return el;
};

describe('fleetProblems', () => {
  it('молчит, нет агента, продукт не отвечает, ревизия упала или не дошла; выключенные - нет', () => {
    const ms: Machine[] = [
      runtimeMachine('ok'),
      machine('silent', { runtime_status: 'offline' }),
      machine('fresh', { seen_at: null }),
      runtimeMachine('deg', { runtime_status: 'degraded', telemt_api_ok: false }),
      machine('fail', { observed: [{ resource: 'mtg.config', revision: 3, status: 'failed', error: 'bad', seen_at: NOW_ISO() }] }),
      machine('late', { observed: [{ resource: 'telemt.users', revision: 3, status: 'applied', seen_at: NOW_ISO(), desired_revision: 5 }] }),
      machine('off', { enabled: false, runtime_status: 'offline' }),
    ];
    expect(fleetProblems(ms).map((p) => `${p.machine.code}:${p.kind}:${p.detail ?? ''}`)).toEqual([
      'silent:silent:',
      'fresh:no_agent:',
      'deg:degraded:telemt',
      'fail:apply_failed:bad',
      'late:behind:3 → 5',
    ]);
  });
});

describe('OverviewPage', () => {
  it('плитки: машины, подключения, проблемы, сертификаты', async () => {
    mockFetch((url) => (url === '/api/v1/overview' ? { body: overviewOf(MACHINES) } : undefined));

    renderWithProviders(<OverviewPage />);

    const machines = await screen.findByTestId('tile-machines');
    expect(within(machines).getByText(ru.overview.agentsOnline).closest('.ant-statistic')).toHaveTextContent('2');
    expect(within(machines).getByText(ru.overview.agentsSilent).closest('.ant-statistic')).toHaveTextContent('1');
    expect(within(machines).getByText(ru.overview.total).closest('.ant-statistic')).toHaveTextContent('3');

    const conns = screen.getByTestId('tile-connections');
    expect(within(conns).getByText(ru.overview.total).closest('.ant-statistic')).toHaveTextContent('43');
    expect(within(conns).getByText(ru.overview.usersOnline).closest('.ant-statistic')).toHaveTextContent('9');

    expect(within(screen.getByTestId('tile-problems')).getByText(ru.overview.problemsOpen).closest('.ant-statistic')).toHaveTextContent('2');

    const certs = screen.getByTestId('tile-certs');
    expect(within(certs).getByText('истекают за 14 дней').closest('.ant-statistic')).toHaveTextContent('1');
    expect(within(certs).getByText(ru.overview.certsMissing).closest('.ant-statistic')).toHaveTextContent('1');
  });

  it('список проблем вместо таблицы машин; клик - карточка машины', async () => {
    const fetchFn = mockFetch((url) => {
      if (url === '/api/v1/overview') return { body: overviewOf(MACHINES) };
      if (urlPath(url) === '/api/v1/machines/exit-a1/runtime') return { body: machineRuntime('exit-a1') };
      return undefined;
    });
    const user = userEvent.setup();

    renderWithProviders(<OverviewPage />);
    await within(screen.getByTestId('overview-problems')).findByText(ru.overview.problemKind.degraded);
    expect(within(problemRow('exit-a1', 'degraded')).getByText('telemt')).toBeInTheDocument();
    expect(within(problemRow('exit-new', 'no_agent')).getByText(ru.overview.problemKind.no_agent)).toBeInTheDocument();
    expect(screen.queryByTestId('overview-machines')).not.toBeInTheDocument();

    await user.click(problemRow('exit-a1', 'degraded'));
    await screen.findByTestId('machine-state');
    expect(screen.getByText('Машина exit-a1')).toBeInTheDocument();
    expect(fetchFn.mock.calls.map(([u]) => String(u))).toContain('/api/v1/machines/exit-a1/runtime');
  });

  it('проблем нет', async () => {
    mockFetch((url) =>
      url === '/api/v1/overview'
        ? { body: overviewOf([runtimeMachine('relay-a')], { connections: { total: 0, users_online: 0 } }) }
        : undefined,
    );

    renderWithProviders(<OverviewPage />);

    expect(await screen.findByText(ru.overview.noProblems)).toBeInTheDocument();
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
