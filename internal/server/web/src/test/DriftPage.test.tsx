// Тесты экрана «Дрейф»: только машины, расходящиеся дольше порога.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import type { DriftItem } from '@/api/types';
import * as mocks from '@/api/mocks';
import ru from '@/i18n/ru.json';
import DriftPage, { overThreshold } from '@/pages/drift/DriftPage';
import { mockFetch, renderWithProviders } from './test-utils';

const WAIT = { timeout: 3000 };

function item(target: string, sinceSecAgo: number, now: number): DriftItem {
  return {
    target,
    name: target,
    pair: 'pair-x',
    role: 'server',
    desired_rev: 2,
    applied_rev: 1,
    since: new Date(now - sinceSecAgo * 1000).toISOString(),
    last_error: null,
  };
}

const rowKeys = (container: HTMLElement) =>
  Array.from(container.querySelectorAll<HTMLElement>('tbody tr[data-row-key]')).map((r) => r.dataset.rowKey);

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('overThreshold', () => {
  const now = Date.parse('2026-06-01T12:00:00Z');
  const items = [item('a', 59, now), item('b', 60, now), item('c', 3600, now)];

  it.each([
    { threshold: 0, want: ['a', 'b', 'c'] },
    { threshold: 60, want: ['b', 'c'] }, // ровно порог - уже показываем
    { threshold: 61, want: ['c'] },
    { threshold: 3600, want: ['c'] },
    { threshold: 3601, want: [] },
  ])('порог $threshold с -> $want', ({ threshold, want }) => {
    expect(overThreshold(items, threshold, now).map((d) => d.target)).toEqual(want);
  });

  it('пустой список - пустой результат', () => {
    expect(overThreshold([], 300, now)).toEqual([]);
  });

  it('битая дата since считается как 0 с и при пороге > 0 скрывается', () => {
    const broken = { ...item('x', 0, now), since: 'not-a-date' };
    expect(overThreshold([broken], 1, now)).toEqual([]);
    expect(overThreshold([broken], 0, now)).toHaveLength(1);
  });

  it('since в будущем (сбитые часы) не даёт отрицательную длительность', () => {
    const future = item('f', -600, now);
    expect(overThreshold([future], 1, now)).toEqual([]);
  });
});

describe('DriftPage', () => {
  it('порог по умолчанию 5 мин - видны все три расходящиеся машины', async () => {
    mockFetch((url) => (url === '/api/role' ? { body: { role: 'master', is_master: true } } : undefined));

    const { container } = renderWithProviders(<DriftPage />);

    expect(await screen.findByText('srv-b2', {}, WAIT)).toBeInTheDocument();
    // Сортировка по длительности: дольше всех - первой.
    expect(rowKeys(container)).toEqual(['srv-b2', 'relay-b', 'srv-b1']);
    expect(screen.getByText(ru.common.mockData)).toBeInTheDocument();
  });

  it('смена порога на 1 ч оставляет одну машину (srv-b2)', async () => {
    mockFetch((url) => (url === '/api/role' ? { body: { role: 'master', is_master: true } } : undefined));
    const user = userEvent.setup();

    const { container } = renderWithProviders(<DriftPage />);
    await screen.findByText('relay-b', {}, WAIT);

    const segmented = container.querySelector<HTMLElement>('.ant-segmented');
    expect(segmented).not.toBeNull();
    await user.click(within(segmented!).getByText('1 ч'));

    await waitFor(() => expect(rowKeys(container)).toEqual(['srv-b2']), WAIT);
    expect(screen.queryByText('relay-b')).not.toBeInTheDocument();
    expect(screen.queryByText('srv-b1')).not.toBeInTheDocument();
  });

  it('клиентский фильтр отсекает машины ниже порога, даже если сервер их вернул', async () => {
    mockFetch((url) => (url === '/api/role' ? { body: { role: 'master', is_master: true } } : undefined));
    // Сервер «забыл» отфильтровать: отдаёт машину, расходящуюся 10 с при пороге 5 мин.
    const now = Date.now();
    vi.spyOn(mocks, 'mockDrift').mockImplementation((threshold = 300) => ({
      threshold_seconds: threshold,
      data: [item('fresh', 10, now), item('old', 1000, now)],
      paging: { total: 2, limit: 20, offset: 0 },
    }));

    const { container } = renderWithProviders(<DriftPage />);

    await waitFor(() => expect(rowKeys(container)).toEqual(['old']), WAIT);
  });

  it('нет расхождений дольше порога - «Расхождений нет»', async () => {
    mockFetch((url) => (url === '/api/role' ? { body: { role: 'master', is_master: true } } : undefined));
    vi.spyOn(mocks, 'mockDrift').mockImplementation((threshold = 300) => ({
      threshold_seconds: threshold,
      data: [],
      paging: { total: 0, limit: 20, offset: 0 },
    }));

    renderWithProviders(<DriftPage />);

    expect(await screen.findByText(ru.drift.none, {}, WAIT)).toBeInTheDocument();
  });

  it('реплика: кнопки «Применить сейчас» неактивны', async () => {
    mockFetch((url) => (url === '/api/role' ? { body: { role: 'replica', is_master: false } } : undefined));

    renderWithProviders(<DriftPage />);
    await screen.findByText('srv-b2', {}, WAIT);

    expect(screen.getByRole('button', { name: `${ru.pairs.applyNow} srv-b2` })).toBeDisabled();
  });

  it('мастер: «Применить сейчас» - сообщение «Запрошено»', async () => {
    mockFetch((url) => (url === '/api/role' ? { body: { role: 'master', is_master: true } } : undefined));
    const user = userEvent.setup();

    renderWithProviders(<DriftPage />);
    await screen.findByText('srv-b2', {}, WAIT);
    const btn = screen.getByRole('button', { name: `${ru.pairs.applyNow} srv-b2` });
    await waitFor(() => expect(btn).toBeEnabled());
    await user.click(btn);

    expect(await screen.findByText('Запрошено: srv-b2', {}, WAIT)).toBeInTheDocument();
  });
});
