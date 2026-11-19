// Машины как ноды 3x-ui (ADR 0007) и сборка конфигов (ADR 0006): включение/выключение и
// удаление машины с разбором 409, проверка связи, графики нагрузки, лента тревог,
// вкладка «Сборка конфига» с переключением источника и 409 render_mismatch.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { fetchAllEvents } from '@/api/queries';
import type { MachineCheck, MachineEvent, ServerRenderItem } from '@/api/types';
import ru from '@/i18n/ru.json';
import { MachineActions } from '@/components/MachineActions';
import { CheckTab, MetricsTab, formatBps } from '@/components/MachineOps';
import { niceMax, segments } from '@/components/LineChart';
import EventsPage, { filterEvents } from '@/pages/events/EventsPage';
import RenderTab, { renderErrorCode } from '@/pages/servers/RenderTab';
import { urlParam, urlPath } from './fleet-fixtures';
import { server } from './server-fixtures';
import { mockFetch, renderWithProviders } from './test-utils';

afterEach(() => {
  vi.unstubAllGlobals();
});

const MASTER = { status: 200, body: { role: 'master', is_master: true } };
const callsOf = (fn: ReturnType<typeof mockFetch>, method: string, path: string) =>
  fn.mock.calls.filter(([u, init]) => (init?.method ?? 'GET') === method && urlPath(String(u)) === path);

describe('чистые функции', () => {
  it('formatBps выбирает единицы', () => {
    const u: [string, string, string, string] = ['b', 'K', 'M', 'G'];
    expect(formatBps(950, u)).toBe('950 b');
    expect(formatBps(12_340, u)).toBe('12.3 K');
    expect(formatBps(4_100_000, u)).toBe('4.1 M');
    expect(formatBps(1_200_000_000, u)).toBe('1.2 G');
  });

  it('niceMax округляет верх шкалы до 1-2-5', () => {
    expect(niceMax(0)).toBe(1);
    expect(niceMax(7)).toBe(10);
    expect(niceMax(130)).toBe(200);
    expect(niceMax(4100)).toBe(5000);
  });

  it('segments рвёт ряд на null', () => {
    expect(segments([1, 2, null, 3, null, null, 4])).toEqual([[[0, 1], [1, 2]], [[3, 3]], [[6, 4]]]);
    expect(segments([null])).toEqual([]);
  });

  it('renderErrorCode берёт код до двоеточия', () => {
    expect(renderErrorCode('not_assigned: ресурс не назначен')).toBe('not_assigned');
    expect(renderErrorCode(null)).toBeNull();
    expect(renderErrorCode('shared_machine')).toBe('shared_machine');
  });

  it('filterEvents фильтрует по машине и виду', () => {
    const ev: MachineEvent[] = [
      { id: 1, at: '2026-06-01T00:00:00Z', machine: 'a', kind: 'down', detail: '' },
      { id: 2, at: '2026-06-01T00:01:00Z', machine: 'b', kind: 'up', detail: '' },
    ];
    expect(filterEvents(ev, 'a').map((e) => e.id)).toEqual([1]);
    expect(filterEvents(ev, undefined, 'up').map((e) => e.id)).toEqual([2]);
    expect(filterEvents(ev)).toHaveLength(2);
  });
});

describe('fetchAllEvents', () => {
  it('забирает ленту страницами по after_id и отдаёт новые первыми', async () => {
    const all: MachineEvent[] = Array.from({ length: 1200 }, (_, i) => ({
      id: i + 1,
      at: '2026-06-01T00:00:00Z',
      machine: 'm',
      kind: 'down',
      detail: '',
    }));
    const fn = mockFetch((url) => {
      const after = Number(urlParam(url, 'after_id'));
      const limit = Number(urlParam(url, 'limit'));
      const rest = all.filter((e) => e.id > after);
      return { body: { data: rest.slice(0, limit), paging: { total: rest.length, limit, offset: 0 } } };
    });
    const res = await fetchAllEvents();
    expect(res.truncated).toBe(false);
    expect(res.data).toHaveLength(1200);
    expect(res.data[0].id).toBe(1200);
    expect(fn.mock.calls.map(([u]) => urlParam(String(u), 'after_id'))).toEqual(['0', '500', '1000']);
  });
});

describe('MachineActions', () => {
  it('выключение - через подтверждение, PUT {enabled:false}', async () => {
    const user = userEvent.setup();
    const fn = mockFetch((url, init) => {
      if (url === '/api/role') return MASTER;
      if (urlPath(url) === '/api/v1/targets/pr1' && init?.method === 'PUT')
        return { body: { code: 'pr1', kind: 'relay', resources: [], enabled: false } };
      return undefined;
    });
    renderWithProviders(<MachineActions m={{ code: 'pr1', enabled: true }} />);
    const sw = await screen.findByRole('switch', { name: 'Машина pr1 включена' });
    await waitFor(() => expect(sw).toBeEnabled());
    await user.click(sw);
    expect(callsOf(fn, 'PUT', '/api/v1/targets/pr1')).toHaveLength(0);
    await user.click(await screen.findByRole('button', { name: ru.machine.disable }));
    await waitFor(() => expect(callsOf(fn, 'PUT', '/api/v1/targets/pr1')).toHaveLength(1));
    expect(JSON.parse(String(callsOf(fn, 'PUT', '/api/v1/targets/pr1')[0][1]?.body))).toEqual({ enabled: false });
  });

  it('включённую машину удалить нельзя', async () => {
    mockFetch((url) => (url === '/api/role' ? MASTER : undefined));
    renderWithProviders(<MachineActions m={{ code: 'pr1', enabled: true }} />);
    await waitFor(() => expect(screen.getByRole('switch')).toBeEnabled());
    expect(screen.getByRole('button', { name: 'Удалить pr1' })).toBeDisabled();
  });

  it('409 machine_in_use - окно с перечнем ссылок от бэкенда', async () => {
    const user = userEvent.setup();
    mockFetch((url, init) => {
      if (url === '/api/role') return MASTER;
      if (init?.method === 'DELETE')
        return { status: 409, body: { code: 'machine_in_use', message: 'серверы 3, 5; машины ps3' } };
      return undefined;
    });
    renderWithProviders(<MachineActions m={{ code: 'pr3', enabled: false }} />);
    const del = screen.getByRole('button', { name: 'Удалить pr3' });
    await waitFor(() => expect(del).toBeEnabled());
    await user.click(del);
    await user.click(await screen.findByRole('button', { name: ru.common.delete }));
    expect(await screen.findByText(/серверы 3, 5; машины ps3/)).toBeInTheDocument();
    expect(screen.getAllByText('Не удалось удалить pr3').length).toBeGreaterThan(0);
  });
});

describe('CheckTab', () => {
  it('показывает агента и точки входа с TCP и TLS', async () => {
    const user = userEvent.setup();
    const res: MachineCheck = {
      code: 'pr1',
      kind: 'relay',
      enabled: true,
      checked_at: '2026-06-01T12:00:00Z',
      ok: false,
      agent: { seen_at: '2026-06-01T11:59:30Z', age_seconds: 30, fresh: true, silent: false, stale_after_seconds: 120, silent_after_seconds: 600 },
      endpoints: [
        { server_no: 1, server_name: 'Финляндия', endpoint_id: 7, protocol: 'ee', host: 'a.example.net', port: 443, via_relay: true, tcp: { ok: true, ms: 12 }, tls: null },
        {
          server_no: 1,
          server_name: 'Финляндия',
          endpoint_id: 8,
          protocol: 'web',
          host: 'w.example.net',
          port: 443,
          via_relay: true,
          tcp: { ok: true, ms: 9 },
          tls: { ok: false, ms: 30, error: 'x509: expired', not_after: null, days_left: null },
        },
      ],
      truncated: false,
    };
    const fn = mockFetch((url, init) => {
      if (url === '/api/role') return MASTER;
      if (urlPath(url) === '/api/v1/machines/pr1/check' && init?.method === 'POST') return { body: res };
      return undefined;
    });
    renderWithProviders(<CheckTab code="pr1" />);
    const btn = screen.getByRole('button', { name: new RegExp(ru.check.run) });
    await waitFor(() => expect(btn).toBeEnabled());
    // Проверка - только по кнопке.
    expect(callsOf(fn, 'POST', '/api/v1/machines/pr1/check')).toHaveLength(0);
    await user.click(btn);
    expect(await screen.findByText(ru.check.someFailed)).toBeInTheDocument();
    expect(screen.getByText('a.example.net:443')).toBeInTheDocument();
    expect(screen.getByText('ок, 12 мс')).toBeInTheDocument();
    expect(screen.getByText(ru.check.fail)).toBeInTheDocument();
    expect(screen.getByText(ru.check.fresh)).toBeInTheDocument();
  });
});

describe('MetricsTab', () => {
  it('запрашивает окно 24 ч с шагом 5m и рисует три графика; 7 дней - шаг 1h', async () => {
    const user = userEvent.setup();
    const point = (at: string) => ({ at, cpu: 10, mem: 50, disk: 30, load1: 0.2, rx_bps: 1e6, tx_bps: 2e6, connections: 40, users_online: 7 });
    const fn = mockFetch((url) => {
      if (urlPath(url) === '/api/v1/machines/pr1/metrics')
        return { body: { code: 'pr1', from: '', to: '', step: urlParam(url, 'step'), data: [point('2026-06-01T10:00:00Z'), point('2026-06-01T10:05:00Z')] } };
      return undefined;
    });
    renderWithProviders(<MetricsTab code="pr1" />);
    expect(await screen.findByTestId('chart-host')).toBeInTheDocument();
    expect(screen.getByTestId('chart-net')).toBeInTheDocument();
    expect(screen.getByTestId('chart-conns')).toBeInTheDocument();
    const first = String(fn.mock.calls[0][0]);
    expect(urlParam(first, 'step')).toBe('5m');
    const span = Date.parse(urlParam(first, 'to')!) - Date.parse(urlParam(first, 'from')!);
    expect(span).toBe(24 * 3600_000 + 60_000);
    await user.click(screen.getByText(ru.metrics.range['7d']));
    await waitFor(() => expect(fn.mock.calls.some(([u]) => urlParam(String(u), 'step') === '1h')).toBe(true));
  });

  it('пустая история - понятное сообщение', async () => {
    mockFetch((url) =>
      urlPath(url) === '/api/v1/machines/pr1/metrics' ? { body: { code: 'pr1', from: '', to: '', step: '5m', data: [] } } : undefined,
    );
    renderWithProviders(<MetricsTab code="pr1" />);
    expect(await screen.findByText(ru.metrics.empty)).toBeInTheDocument();
  });
});

describe('EventsPage', () => {
  it('новые события первыми, фильтр по виду', async () => {
    const user = userEvent.setup();
    const ev: MachineEvent[] = [
      { id: 1, at: '2026-06-01T10:00:00Z', machine: 'pr4', kind: 'down', detail: 'агент молчит' },
      { id: 2, at: '2026-06-01T11:00:00Z', machine: 'pr4', kind: 'up', detail: 'снова отчитывается' },
    ];
    mockFetch((url) => (urlPath(url) === '/api/v1/events' ? { body: { data: ev, paging: { total: 2, limit: 500, offset: 0 } } } : undefined));
    renderWithProviders(<EventsPage />);
    await screen.findByText('снова отчитывается');
    const rows = document.querySelectorAll('tbody tr[data-row-key]');
    expect([...rows].map((r) => r.getAttribute('data-row-key'))).toEqual(['2', '1']);
    await user.click(screen.getByRole('combobox', { name: ru.events.filterKind }));
    await user.click((await screen.findAllByTitle(ru.events.kinds.down)).at(-1)!);
    await waitFor(() => expect(screen.queryByText('снова отчитывается')).not.toBeInTheDocument());
    expect(screen.getByText('агент молчит')).toBeInTheDocument();
  });
});

describe('RenderTab', () => {
  const items: ServerRenderItem[] = [
    {
      target: 'pr1',
      resource: 'mtg.config',
      candidate_sha: 'aaa',
      current_sha: 'bbb',
      equal: false,
      error: null,
      rendered_at: '2026-06-01T12:00:00Z',
      diff: '--- a\n+++ b\n@@ -1 +1 @@\n-old = 1\n+new = 2',
    },
    {
      target: 'pr1',
      resource: 'telemt.config',
      candidate_sha: 'ccc',
      current_sha: null,
      equal: false,
      error: 'not_assigned: ресурс не назначен',
      rendered_at: '2026-06-01T12:00:00Z',
      diff: '',
    },
  ];

  it('показывает итог сравнения и diff', async () => {
    const user = userEvent.setup();
    mockFetch((url) => {
      if (url === '/api/role') return MASTER;
      if (urlPath(url) === '/api/v1/servers/3/render') return { body: { data: items } };
      return undefined;
    });
    renderWithProviders(<RenderTab server={server(3)} />);
    expect(await screen.findByText('не совпало 2 из 2')).toBeInTheDocument();
    expect(screen.getByText(ru.render.errors.not_assigned)).toBeInTheDocument();
    await user.click(screen.getByText('mtg.config'));
    const diff = await screen.findByTestId('render-diff');
    expect(within(diff).getByText('+new = 2')).toHaveClass('diff-add');
    expect(within(diff).getByText('-old = 1')).toHaveClass('diff-del');
  });

  it('409 render_mismatch - подтверждение и повтор с force', async () => {
    const user = userEvent.setup();
    const fn = mockFetch((url, init) => {
      if (url === '/api/role') return MASTER;
      if (urlPath(url) === '/api/v1/servers/3/render') return { body: { data: items } };
      if (urlPath(url) === '/api/v1/servers/3/config-source' && init?.method === 'PUT') {
        const body = JSON.parse(String(init.body)) as { force: boolean };
        if (!body.force) return { status: 409, body: { code: 'render_mismatch', message: 'pr1 mtg.config не совпал' } };
        return { body: { ...server(3), config_source: 'telemux' } };
      }
      return undefined;
    });
    renderWithProviders(<RenderTab server={server(3)} />);
    expect(screen.getByTestId('config-source')).toHaveTextContent(ru.render.sources.bot);
    const toTm = screen.getByRole('button', { name: ru.render.toTelemux });
    await waitFor(() => expect(toTm).toBeEnabled());
    await user.click(toTm);
    await user.click(await screen.findByRole('button', { name: ru.render.switch }));
    await user.click(await screen.findByRole('button', { name: ru.render.forceOk }));
    await waitFor(() => expect(screen.getByTestId('config-source')).toHaveTextContent(ru.render.sources.telemux));
    const bodies = callsOf(fn, 'PUT', '/api/v1/servers/3/config-source').map(([, i]) => JSON.parse(String(i?.body)));
    expect(bodies).toEqual([
      { source: 'telemux', force: false },
      { source: 'telemux', force: true },
    ]);
  });
});
