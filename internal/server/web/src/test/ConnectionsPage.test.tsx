// Тесты «Подключений»: сводка по машинам и продуктам, пользователи онлайн, фильтры
// (машина, продукт, минимум подключений, поиск по пользователю), активные IP
// раскрываются списком, пагинация с сервера, ошибки 401 и 500, обновление раз в 15 с.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { CONNECTIONS_REFRESH_MS } from '@/api/queries';
import type { Connection, ConnectionSummary } from '@/api/types';
import ru from '@/i18n/ru.json';
import ConnectionsPage, { groupSummary } from '@/pages/connections/ConnectionsPage';
import { NOW_ISO, connection, pageOf, urlParam, urlPath } from './fleet-fixtures';
import { mockFetch, renderWithProviders } from './test-utils';

afterEach(() => {
  vi.unstubAllGlobals();
});

const SUMMARY: ConnectionSummary[] = [
  { machine: 'relay-a', product: 'telemt', connections_total: 340, users_online: 118, seen_at: NOW_ISO(), stale: false, api_ok: true },
  {
    machine: 'relay-a',
    product: 'mtg',
    connections_total: 0,
    users_online: 0,
    seen_at: NOW_ISO(),
    stale: true,
    api_ok: false,
    error: 'connection refused',
  },
];

const CONNS: Connection[] = [
  connection('relay-a', 'alice_1', { connections: 3 }),
  connection('relay-a', 'bob_2', {
    product: 'mtg',
    connections: 1,
    active_ips: [],
    bytes_in: 1536,
    bytes_out: 5 * 1024 * 1024,
    last_seen: NOW_ISO(),
  }),
];

// Бэкенд: фильтры /api/v1/connections применяются как на мастере.
function backend(conns: Connection[] = CONNS, summary: ConnectionSummary[] = SUMMARY) {
  return (url: string) => {
    if (urlPath(url) === '/api/v1/connections/summary') return { body: pageOf(summary, url) };
    if (urlPath(url) === '/api/v1/connections') {
      const machine = urlParam(url, 'machine');
      const product = urlParam(url, 'product');
      const min = Number(urlParam(url, 'min_connections') ?? 0);
      const user = urlParam(url, 'user') ?? '';
      const rows = conns.filter(
        (c) =>
          (!machine || c.machine === machine) && (!product || c.product === product) && c.connections >= min && c.key.startsWith(user),
      );
      return { body: pageOf(rows, url) };
    }
    return undefined;
  };
}

const connCalls = (fn: ReturnType<typeof mockFetch>) =>
  fn.mock.calls.map(([u]) => String(u)).filter((u) => urlPath(u) === '/api/v1/connections');

const usersCard = () => screen.getByTestId('connections-users');

describe('ConnectionsPage', () => {
  it('сводка: одна строка на машину, telemt и mtg рядом; API и свежесть', async () => {
    mockFetch(backend());

    renderWithProviders(<ConnectionsPage />);

    const summary = screen.getByTestId('connections-summary');
    expect(await within(summary).findByText('340 подкл. · 118 онлайн')).toBeInTheDocument();
    expect(within(summary).getByText('0 подкл. · 0 онлайн')).toBeInTheDocument();
    expect(summary.querySelectorAll('tbody tr[data-row-key]')).toHaveLength(1);
    expect(within(summary).getByText(ru.drawer.apiDown)).toBeInTheDocument();
    expect(within(summary).getByText(ru.connections.stale)).toBeInTheDocument();
  });

  it('groupSummary: строки машины склеиваются, свежесть - самая старая', () => {
    const old = '2026-01-01T00:00:00Z';
    const g = groupSummary([SUMMARY[0], { ...SUMMARY[1], seen_at: old }, { ...SUMMARY[0], machine: 'ps5' }]);
    expect(g.map((m) => [m.machine, !!m.telemt, !!m.mtg, m.stale])).toEqual([
      ['relay-a', true, true, true],
      ['ps5', true, false, false],
    ]);
    expect(g[0].seen_at).toBe(old);
  });

  it('пользователи онлайн: по умолчанию min_connections=1, трафик человекочитаемо', async () => {
    const fetchFn = mockFetch(backend());

    renderWithProviders(<ConnectionsPage />);

    const card = usersCard();
    expect(await within(card).findByText('alice_1')).toBeInTheDocument();
    expect(within(card).getByText('bob_2')).toBeInTheDocument();
    expect(within(card).getByText('1.5 KB / 5 MB')).toBeInTheDocument();
    expect(connCalls(fetchFn)[0]).toBe('/api/v1/connections?min_connections=1&limit=20&offset=0');
  });

  it('активные IP раскрываются списком', async () => {
    mockFetch(backend());
    const user = userEvent.setup();

    renderWithProviders(<ConnectionsPage />);
    const card = usersCard();
    await within(card).findByText('alice_1');
    expect(screen.queryByText('192.0.2.10')).not.toBeInTheDocument();

    // У bob_2 адресов нет - раскрывать нечего: кнопка одна.
    const expanders = within(card).getAllByRole('button', { name: /развернуть|expand/i });
    expect(expanders).toHaveLength(1);
    await user.click(expanders[0]);

    const ips = await screen.findByTestId('ips-relay-a/telemt/alice_1');
    expect(within(ips).getByText('192.0.2.10')).toBeInTheDocument();
    expect(within(ips).getByText('198.51.100.7')).toBeInTheDocument();
  });

  it('поиск по пользователю - параметр user (префикс)', async () => {
    const fetchFn = mockFetch(backend());
    const user = userEvent.setup();

    renderWithProviders(<ConnectionsPage />);
    const card = usersCard();
    await within(card).findByText('alice_1');

    await user.type(within(card).getByRole('textbox', { name: ru.connections.search }), 'bo');

    await waitFor(() => expect(within(card).queryByText('alice_1')).not.toBeInTheDocument());
    expect(within(card).getByText('bob_2')).toBeInTheDocument();
    expect(connCalls(fetchFn).at(-1)).toBe('/api/v1/connections?min_connections=1&user=bo&limit=20&offset=0');
  });

  it('фильтры машины, продукта и минимума подключений', async () => {
    const fetchFn = mockFetch(backend());
    const user = userEvent.setup();

    renderWithProviders(<ConnectionsPage />);
    const card = usersCard();
    await within(card).findByText('alice_1');

    await user.click(within(card).getByRole('combobox', { name: ru.connections.machine }));
    await user.click(await screen.findByTitle('relay-a'));
    await waitFor(() => expect(connCalls(fetchFn).some((u) => urlParam(u, 'machine') === 'relay-a')).toBe(true));

    await user.click(within(card).getByRole('combobox', { name: ru.connections.product }));
    await user.click(await screen.findByTitle('mtg-multi'));
    await waitFor(() => expect(within(card).queryByText('alice_1')).not.toBeInTheDocument());
    expect(within(card).getByText('bob_2')).toBeInTheDocument();

    const min = within(card).getByRole('spinbutton', { name: ru.connections.minConnections });
    await user.clear(min);
    await user.type(min, '2');
    await waitFor(() => expect(within(card).queryByText('bob_2')).not.toBeInTheDocument());
    const last = connCalls(fetchFn).at(-1) ?? '';
    expect(urlParam(last, 'machine')).toBe('relay-a');
    expect(urlParam(last, 'product')).toBe('mtg');
    expect(urlParam(last, 'min_connections')).toBe('2');
    expect(await within(card).findByText(ru.connections.empty)).toBeInTheDocument();
  });

  it('пагинация с сервера: вторая страница - offset=20', async () => {
    const many = Array.from({ length: 25 }, (_, i) => connection('relay-a', `u_${String(i + 1).padStart(2, '0')}`));
    const fetchFn = mockFetch(backend(many));
    const user = userEvent.setup();

    renderWithProviders(<ConnectionsPage />);
    const card = usersCard();
    await within(card).findByText('u_01');
    expect(within(card).getByText('Всего 25')).toBeInTheDocument();

    await user.click(within(card).getByRole('button', { name: 'Страница 2' }));

    expect(await within(card).findByText('u_21')).toBeInTheDocument();
    expect(connCalls(fetchFn)).toContain('/api/v1/connections?min_connections=1&limit=20&offset=20');
  });

  it('пусто - «Подключений нет» и «Отчётов пока нет»', async () => {
    mockFetch(backend([], []));

    renderWithProviders(<ConnectionsPage />);

    expect(await within(usersCard()).findByText(ru.connections.empty)).toBeInTheDocument();
    expect(await within(screen.getByTestId('connections-summary')).findByText(ru.connections.summaryEmpty)).toBeInTheDocument();
  });

  it('401 - «сессия истекла»', async () => {
    mockFetch((url) =>
      url.startsWith('/api/v1/connections') ? { status: 401, body: { code: 'unauthorized', message: 'unauthorized' } } : undefined,
    );

    renderWithProviders(<ConnectionsPage />);

    expect(await within(usersCard()).findByText(ru.errors.unauthorized)).toBeInTheDocument();
  });

  it('500 - понятный текст без деталей базы', async () => {
    mockFetch((url) => (url.startsWith('/api/v1/connections') ? { status: 500, body: { code: 'internal', message: 'pq: boom' } } : undefined));

    renderWithProviders(<ConnectionsPage />);

    expect(await within(usersCard()).findByText('Ошибка сервера (500). Подробности - в журнале демона.')).toBeInTheDocument();
    expect(screen.queryByText(/pq: boom/)).not.toBeInTheDocument();
  });

  it('обновляется раз в 15 секунд', async () => {
    mockFetch(backend());

    const { queryClient } = renderWithProviders(<ConnectionsPage />);
    await within(usersCard()).findByText('alice_1');

    const qs = queryClient.getQueryCache().findAll({ queryKey: ['connections'] });
    expect(qs.length).toBeGreaterThan(0);
    for (const q of qs) expect((q.options as { refetchInterval?: unknown }).refetchInterval).toBe(CONNECTIONS_REFRESH_MS);
  });
});
