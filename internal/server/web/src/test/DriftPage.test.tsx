// Тесты экрана «Дрейф»: ответы /api/v1/drift по схеме api/openapi.yaml через подменённый fetch.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { FLEET_REFRESH_MS, qk } from '@/api/queries';
import type { DriftItem } from '@/api/types';
import ru from '@/i18n/ru.json';
import DriftPage, { DEFAULT_THRESHOLD, driftKey } from '@/pages/drift/DriftPage';
import { driftItem, pageOf, urlParam, urlPath } from './fleet-fixtures';
import { mockFetch, renderWithProviders } from './test-utils';

const ITEMS: DriftItem[] = [
  driftItem('exit-a1', { relay_code: 'relay-a' }),
  driftItem('exit-a2', {
    reason: 'failed',
    relay_code: 'relay-a',
    observed_revision: 8,
    error: 'telemt: config rejected',
  }),
  driftItem('relay-b', {
    kind: 'relay',
    reason: 'silent',
    resource: '',
    desired_revision: 0,
    observed_revision: null,
    seen_at: null,
  }),
];

// Бэкенд: строки фильтруются по порогу (для теста - silent только при пороге <= 600).
function backend(items: DriftItem[]) {
  return (url: string) => {
    if (urlPath(url) !== '/api/v1/drift') return undefined;
    const threshold = Number(urlParam(url, 'threshold'));
    const visible = items.filter((d) => d.reason !== 'silent' || threshold <= 600);
    return { body: { ...pageOf(visible, url), threshold_seconds: threshold } };
  };
}

const row = (d: DriftItem) => {
  const el = document.querySelector<HTMLElement>(`tr[data-row-key="${driftKey(d)}"]`);
  if (!el) throw new Error(`нет строки ${driftKey(d)}`);
  return el;
};
const driftCalls = (fn: ReturnType<typeof mockFetch>) =>
  fn.mock.calls.map(([u]) => String(u)).filter((u) => u.startsWith('/api/v1/drift'));

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('DriftPage', () => {
  it('по умолчанию порог 600 с, первая страница limit=20&offset=0', async () => {
    const fetchFn = mockFetch(backend(ITEMS));

    renderWithProviders(<DriftPage />);
    await screen.findByText('exit-a1');

    expect(DEFAULT_THRESHOLD).toBe(600);
    expect(driftCalls(fetchFn)[0]).toBe('/api/v1/drift?threshold=600&limit=20&offset=0');
    expect(screen.getByRole('radio', { name: '10 мин' })).toBeChecked();
  });

  it('причины, ресурс, ревизии, релей и ошибка', async () => {
    mockFetch(backend(ITEMS));

    renderWithProviders(<DriftPage />);
    await screen.findByText('exit-a1');

    const behind = row(ITEMS[0]);
    expect(within(behind).getByText(ru.driftReason.behind)).toBeInTheDocument();
    expect(within(behind).getByText('telemt.users')).toBeInTheDocument();
    expect(within(behind).getByTestId(`rev-${driftKey(ITEMS[0])}`)).toHaveTextContent('7 / 8');
    expect(within(behind).getByText('релей relay-a')).toBeInTheDocument();
    expect(within(behind).getByText(ru.drift.noError)).toBeInTheDocument();
    expect(within(behind).getByText('1 ч')).toBeInTheDocument();

    const failed = row(ITEMS[1]);
    expect(within(failed).getByText(ru.driftReason.failed)).toBeInTheDocument();
    expect(within(failed).getByText('telemt: config rejected')).toBeInTheDocument();

    const silent = row(ITEMS[2]);
    expect(within(silent).getByText(ru.driftReason.silent)).toBeInTheDocument();
    expect(within(silent).getByText(ru.fleet.noAgent)).toBeInTheDocument();
    expect(within(silent).queryByTestId(`rev-${driftKey(ITEMS[2])}`)).not.toBeInTheDocument();
  });

  it('смена порога - новый запрос с threshold и с первой страницы', async () => {
    const fetchFn = mockFetch(backend(ITEMS));
    const user = userEvent.setup();

    renderWithProviders(<DriftPage />);
    await screen.findByText('relay-b');

    // Сам input у Segmented прозрачен для мыши - кликаем по подписи.
    await user.click(screen.getByRole('radio', { name: '1 ч' }).closest('label')!);
    await waitFor(() => expect(driftCalls(fetchFn)).toContain('/api/v1/drift?threshold=3600&limit=20&offset=0'));
    await waitFor(() => expect(screen.queryByText('relay-b')).not.toBeInTheDocument());
    expect(screen.getByText('exit-a1')).toBeInTheDocument();
  });

  it('пустой ответ - «Расхождений нет» с порогом', async () => {
    mockFetch(backend([]));

    renderWithProviders(<DriftPage />);

    expect(await screen.findByText(ru.drift.noneHint.replace('{{threshold}}', '10 мин'))).toBeInTheDocument();
    expect(screen.getByText(ru.drift.none)).toBeInTheDocument();
  });

  it('пагинация по paging.total: вторая страница - offset=20', async () => {
    const many = Array.from({ length: 30 }, (_, i) => driftItem(`exit-${String(i + 1).padStart(2, '0')}`));
    const fetchFn = mockFetch(backend(many));
    const user = userEvent.setup();

    renderWithProviders(<DriftPage />);
    await screen.findByText('exit-01');
    expect(screen.getByText('Всего 30')).toBeInTheDocument();
    expect(screen.queryByText('exit-21')).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Страница 2' }));

    expect(await screen.findByText('exit-21')).toBeInTheDocument();
    expect(screen.queryByText('exit-01')).not.toBeInTheDocument();
    expect(driftCalls(fetchFn)).toContain('/api/v1/drift?threshold=600&limit=20&offset=20');
  });

  it('401 - «сессия истекла»', async () => {
    mockFetch((url) =>
      urlPath(url) === '/api/v1/drift' ? { status: 401, body: { code: 'unauthorized', message: 'unauthorized' } } : undefined,
    );

    renderWithProviders(<DriftPage />);

    expect(await screen.findByText(ru.errors.unauthorized)).toBeInTheDocument();
    expect(screen.queryByText(ru.drift.none)).not.toBeInTheDocument();
  });

  it('500 - понятный текст без деталей и «Повторить»', async () => {
    mockFetch((url) =>
      urlPath(url) === '/api/v1/drift' ? { status: 500, body: { code: 'internal', message: 'pq: timeout' } } : undefined,
    );

    renderWithProviders(<DriftPage />);

    expect(await screen.findByText('Ошибка сервера (500). Подробности - в журнале демона.')).toBeInTheDocument();
    expect(screen.queryByText(/pq: timeout/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: ru.common.retry })).toBeInTheDocument();
  });

  it('нет кнопки «применить сейчас»', async () => {
    mockFetch(backend(ITEMS));

    renderWithProviders(<DriftPage />);
    await screen.findByText('exit-a1');

    expect(screen.queryByRole('button', { name: /Применить/ })).not.toBeInTheDocument();
  });

  it('данные обновляются раз в 30 секунд', async () => {
    mockFetch(backend(ITEMS));

    const { queryClient } = renderWithProviders(<DriftPage />);
    await screen.findByText('exit-a1');

    const q = queryClient.getQueryCache().find({ queryKey: qk.drift(600, 20, 0) });
    expect((q?.options as { refetchInterval?: unknown }).refetchInterval).toBe(FLEET_REFRESH_MS);
  });
});
