// Тесты экрана «Пары»: ответы /api/v1/pairs и /api/v1/targets/{code}/apply-log по схеме
// api/openapi.yaml через подменённый fetch.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { FLEET_REFRESH_MS, qk } from '@/api/queries';
import type { Pair } from '@/api/types';
import { resourceRows } from '@/components/FleetTags';
import ru from '@/i18n/ru.json';
import { NO_RELAY_KEY, pairMachines } from '@/pages/pairs/PairTable';
import PairsPage, { statusCounts } from '@/pages/pairs/PairsPage';
import { logEntry, machine, pageOf, pairMachine, urlPath } from './fleet-fixtures';
import { mockFetch, renderWithProviders } from './test-utils';

const PAIRS: Pair[] = [
  {
    relay: pairMachine('relay-a', {
      kind: 'relay',
      via: 'exit-a1',
      resources: ['telemt.users', 'mtg.config'],
      desired: [
        { resource: 'telemt.users', revision: 8 },
        { resource: 'mtg.config', revision: 3 },
      ],
      observed: [
        { resource: 'telemt.users', revision: 7, status: 'applied', seen_at: new Date().toISOString() },
        { resource: 'mtg.config', revision: 3, status: 'applied', seen_at: new Date().toISOString() },
      ],
      status: 'behind',
    }),
    exits: [
      pairMachine('exit-a1', {
        relay_code: 'relay-a',
        resources: [],
        desired: [],
        observed: [],
        checks: [{ name: 'bridge', ok: true }],
      }),
      pairMachine('exit-a2', {
        relay_code: 'relay-a',
        status: 'failed',
        observed: [
          { resource: 'telemt.users', revision: 7, status: 'failed', error: 'telemt: reload refused', seen_at: new Date().toISOString() },
        ],
        checks: [{ name: 'bridge', ok: false, error: 'dial tcp: connection refused' }],
      }),
      pairMachine('exit-a3', { relay_code: 'relay-a', status: 'silent', seen_at: new Date(Date.now() - 3_600_000).toISOString() }),
      pairMachine('exit-a4', { relay_code: 'relay-a', status: 'disabled', enabled: false }),
    ],
  },
  {
    relay: null,
    exits: [
      pairMachine('panel-1', { kind: 'panel', status: 'no_agent', seen_at: null, agent_version: undefined, observed: [] }),
    ],
  },
];

function backend(pairs: Pair[], log: Record<string, ReturnType<typeof logEntry>[]> = {}) {
  return (url: string) => {
    if (url === '/api/role') return { body: { role: 'master', is_master: true } };
    if (urlPath(url) === '/api/v1/pairs') return { body: pageOf(pairs, url) };
    const m = /^\/api\/v1\/targets\/([^/]+)\/apply-log$/.exec(urlPath(url));
    if (m) {
      const code = decodeURIComponent(m[1]);
      if (!(code in log)) return { status: 404, body: { code: 'not_found', message: `машина ${code} не найдена` } };
      return { body: pageOf(log[code], url) };
    }
    return undefined;
  };
}

const card = (key: string) => screen.getByTestId(`pair-${key}`);
const rowOf = (code: string) => {
  const row = document.querySelector<HTMLElement>(`tr[data-row-key="${code}"]`);
  if (!row) throw new Error(`нет строки ${code}`);
  return row;
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('PairsPage', () => {
  it('пара: релей и выходы, первый запрос limit=20&offset=0', async () => {
    const fetchFn = mockFetch(backend(PAIRS));

    renderWithProviders(<PairsPage />);

    expect(await screen.findByRole('button', { name: ru.fleet.historyOf.replace('{{code}}', 'relay-a') })).toBeInTheDocument();
    const a = card('relay-a');
    for (const code of ['relay-a', 'exit-a1', 'exit-a2', 'exit-a3', 'exit-a4']) {
      expect(within(a).getByRole('button', { name: ru.fleet.historyOf.replace('{{code}}', code) })).toBeInTheDocument();
    }
    const calls = fetchFn.mock.calls.map(([u]) => String(u)).filter((u) => u.startsWith('/api/v1/pairs'));
    expect(calls[0]).toBe('/api/v1/pairs?limit=20&offset=0');
  });

  it('статусы машин - цветом и подписью', async () => {
    mockFetch(backend(PAIRS));

    renderWithProviders(<PairsPage />);
    await screen.findByTestId('pair-relay-a');

    const status = (code: string) => rowOf(code).querySelector('[data-status]')?.getAttribute('data-status');
    expect(status('relay-a')).toBe('behind');
    expect(status('exit-a1')).toBe('ok');
    expect(status('exit-a2')).toBe('failed');
    expect(status('exit-a3')).toBe('silent');
    expect(status('exit-a4')).toBe('disabled');
    expect(status('panel-1')).toBe('no_agent');
    expect(within(rowOf('relay-a')).getByText(ru.status.behind)).toBeInTheDocument();
    expect(within(rowOf('exit-a2')).getByText(ru.status.failed)).toBeInTheDocument();
    expect(within(rowOf('panel-1')).getByText(ru.status.no_agent)).toBeInTheDocument();
    // Сводка пары - счётчики по проблемным статусам.
    expect(within(card('relay-a')).getByText(`${ru.status.failed}: 1`)).toBeInTheDocument();
  });

  it('ресурсы с применённой/желаемой ревизией, мост, проверки и ошибки агента', async () => {
    mockFetch(backend(PAIRS));

    renderWithProviders(<PairsPage />);
    await screen.findByTestId('pair-relay-a');

    const relay = rowOf('relay-a');
    const users = within(relay).getByTestId('res-telemt.users');
    expect(users).toHaveTextContent('telemt.users 7/8');
    expect(users).toHaveAttribute('data-state', 'behind');
    expect(within(relay).getByTestId('res-mtg.config')).toHaveAttribute('data-state', 'applied');
    expect(within(relay).getByText('exit-a1')).toBeInTheDocument(); // мост
    expect(within(relay).getByText('0.6.0')).toBeInTheDocument();

    expect(within(rowOf('exit-a1')).getByTestId('check-bridge')).toHaveTextContent(`bridge: ${ru.fleet.checkOk}`);
    expect(within(rowOf('exit-a1')).getByText(ru.fleet.noResources)).toBeInTheDocument();
    const failed = rowOf('exit-a2');
    expect(within(failed).getByTestId('check-bridge')).toHaveTextContent(`bridge: ${ru.fleet.checkFailed}`);
    expect(within(failed).getByText('dial tcp: connection refused')).toBeInTheDocument();
    expect(within(failed).getByTestId('res-telemt.users')).toHaveAttribute('data-state', 'failed');
    expect(within(failed).getByText('telemt.users: telemt: reload refused')).toBeInTheDocument();
  });

  it('псевдо-пара relay: null подписана «Без релея», агента нет', async () => {
    mockFetch(backend(PAIRS));

    renderWithProviders(<PairsPage />);
    const noRelay = await screen.findByTestId(`pair-${NO_RELAY_KEY}`);

    expect(within(noRelay).getByText(ru.pairs.noRelay)).toBeInTheDocument();
    expect(within(noRelay).getByText(ru.kinds.panel)).toBeInTheDocument();
    expect(within(rowOf('panel-1')).getByText(ru.fleet.noAgent)).toBeInTheDocument();
    expect(within(card('relay-a')).queryByText(ru.pairs.noRelay)).not.toBeInTheDocument();
  });

  it('пустой список - «Пар пока нет»', async () => {
    mockFetch(backend([]));

    renderWithProviders(<PairsPage />);

    expect(await screen.findByText(ru.pairs.empty)).toBeInTheDocument();
    expect(document.querySelector('.ant-pagination')).toBeNull();
  });

  it('401 - понятный текст «сессия истекла»', async () => {
    mockFetch((url) =>
      urlPath(url) === '/api/v1/pairs' ? { status: 401, body: { code: 'unauthorized', message: 'unauthorized' } } : undefined,
    );

    renderWithProviders(<PairsPage />);

    expect(await screen.findByText(ru.errors.loadFailed)).toBeInTheDocument();
    expect(screen.getByText(ru.errors.unauthorized)).toBeInTheDocument();
  });

  it('500 - понятный текст без деталей и кнопка «Повторить»', async () => {
    mockFetch((url) =>
      urlPath(url) === '/api/v1/pairs' ? { status: 500, body: { code: 'internal', message: 'pq: deadlock' } } : undefined,
    );

    renderWithProviders(<PairsPage />);

    expect(await screen.findByText('Ошибка сервера (500). Подробности - в журнале демона.')).toBeInTheDocument();
    expect(screen.queryByText(/deadlock/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: ru.common.retry })).toBeInTheDocument();
  });

  it('пагинация по парам: вторая страница - offset=20', async () => {
    const many: Pair[] = Array.from({ length: 25 }, (_, i) => ({
      relay: pairMachine(`relay-${String(i + 1).padStart(2, '0')}`, { kind: 'relay' }),
      exits: [],
    }));
    const fetchFn = mockFetch(backend(many));
    const user = userEvent.setup();

    renderWithProviders(<PairsPage />);
    await screen.findByTestId('pair-relay-01');
    expect(screen.queryByTestId('pair-relay-21')).not.toBeInTheDocument();
    expect(screen.getByText('Всего 25')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Страница 2' }));

    expect(await screen.findByTestId('pair-relay-21')).toBeInTheDocument();
    expect(screen.queryByTestId('pair-relay-01')).not.toBeInTheDocument();
    expect(fetchFn.mock.calls.map(([u]) => String(u))).toContain('/api/v1/pairs?limit=20&offset=20');
  });

  it('клик по машине открывает её историю применений, постранично', async () => {
    const log = {
      'exit-a2': [
        logEntry(0, { status: 'failed', error: 'telemt: reload refused' }),
        logEntry(1, { resource: 'check:bridge', revision: null, status: 'ok' }),
        ...Array.from({ length: 23 }, (_, i) => logEntry(i + 2)),
      ],
    };
    const fetchFn = mockFetch(backend(PAIRS, log));
    const user = userEvent.setup();

    renderWithProviders(<PairsPage />);
    await user.click(await screen.findByRole('button', { name: ru.fleet.historyOf.replace('{{code}}', 'exit-a2') }));

    const drawer = await screen.findByRole('dialog');
    expect(within(drawer).getByText(ru.history.title.replace('{{code}}', 'exit-a2'))).toBeInTheDocument();
    expect(await within(drawer).findByText('telemt: reload refused')).toBeInTheDocument();
    expect(within(drawer).getByText(ru.history.check)).toBeInTheDocument();
    expect(within(drawer).getByText(ru.applyStatus.ok)).toBeInTheDocument();
    expect(within(drawer).getByText('Всего 25')).toBeInTheDocument();
    const logCalls = () => fetchFn.mock.calls.map(([u]) => String(u)).filter((u) => u.includes('/apply-log'));
    expect(logCalls()[0]).toBe('/api/v1/targets/exit-a2/apply-log?limit=20&offset=0');

    await user.click(within(drawer).getByRole('button', { name: 'Страница 2' }));
    await waitFor(() => expect(logCalls()).toContain('/api/v1/targets/exit-a2/apply-log?limit=20&offset=20'));
    expect(await within(drawer).findByText('80')).toBeInTheDocument(); // ревизия 25-й записи (100 - 20)
  });

  it('история неизвестной машины - 404 с текстом сервера', async () => {
    mockFetch(backend(PAIRS, {}));
    const user = userEvent.setup();

    renderWithProviders(<PairsPage />);
    await user.click(await screen.findByRole('button', { name: ru.fleet.historyOf.replace('{{code}}', 'exit-a1') }));

    const drawer = await screen.findByRole('dialog');
    expect(await within(drawer).findByText('машина exit-a1 не найдена')).toBeInTheDocument();
  });

  it('данные обновляются раз в 30 секунд', async () => {
    mockFetch(backend(PAIRS));

    const { queryClient } = renderWithProviders(<PairsPage />);
    await screen.findByTestId('pair-relay-a');

    const q = queryClient.getQueryCache().find({ queryKey: qk.pairs(20, 0) });
    expect(FLEET_REFRESH_MS).toBe(30_000);
    expect((q?.options as { refetchInterval?: unknown }).refetchInterval).toBe(FLEET_REFRESH_MS);
  });

  it('нет кнопок «применить сейчас» и «повторить» для машин', async () => {
    mockFetch(backend(PAIRS));

    renderWithProviders(<PairsPage />);
    await screen.findByTestId('pair-relay-a');

    expect(screen.queryByRole('button', { name: /Применить/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument();
  });
});

describe('pairMachines, statusCounts, resourceRows', () => {
  it('релей первым, у псевдо-пары - только выходы', () => {
    expect(pairMachines(PAIRS[0]).map((m) => m.code)).toEqual(['relay-a', 'exit-a1', 'exit-a2', 'exit-a3', 'exit-a4']);
    expect(pairMachines(PAIRS[1]).map((m) => m.code)).toEqual(['panel-1']);
  });

  it('счётчики статусов пары', () => {
    expect(statusCounts(PAIRS[0])).toEqual({ behind: 1, ok: 1, failed: 1, silent: 1, disabled: 1 });
  });

  it.each([
    { name: 'применено и совпадает', obs: { revision: 8, status: 'applied' as const }, want: 'applied' },
    { name: 'отстаёт', obs: { revision: 7, status: 'applied' as const }, want: 'behind' },
    { name: 'отказ важнее отставания', obs: { revision: 7, status: 'failed' as const }, want: 'failed' },
    { name: 'режим сравнения', obs: { revision: 8, status: 'compared' as const }, want: 'compared' },
    { name: 'агент не сообщал', obs: null, want: 'missing' },
  ])('$name -> $want', ({ obs, want }) => {
    const observed = obs ? [{ resource: 'r', seen_at: '2026-01-01T00:00:00Z', ...obs }] : [];
    expect(resourceRows(['r'], observed, [{ resource: 'r', revision: 8 }])[0].state).toBe(want);
  });

  it('ресурсы из desired/observed, которых нет в resources, тоже показываются', () => {
    const m = machine('m', { resources: ['a'], observed: [{ resource: 'b', revision: 1, status: 'applied', seen_at: '2026-01-01T00:00:00Z' }] });
    expect(resourceRows(m.resources, m.observed, [{ resource: 'c', revision: 2 }]).map((r) => r.resource)).toEqual(['a', 'c', 'b']);
  });

  it('без желаемого (экран «Машины») - только применённая ревизия', () => {
    const rows = resourceRows(['a'], [{ resource: 'a', revision: 5, status: 'applied', seen_at: '2026-01-01T00:00:00Z' }]);
    expect(rows[0]).toMatchObject({ desired: null, observed: 5, state: 'applied' });
  });
});

