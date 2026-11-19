// Сайдбар и маршруты: порядок разделов, «Обзор» - страница по умолчанию,
// неизвестный путь ведёт на обзор; форматирование байтов и процентов.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, within } from '@testing-library/react';

import ru from '@/i18n/ru.json';
import AppSidebar from '@/layouts/AppSidebar';
import { routes } from '@/routes';
import { formatBytes, formatPct } from '@/utils/format';
import { mockFetch, renderWithProviders } from './test-utils';

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('сайдбар', () => {
  it('разделы по порядку: Обзор, Серверы, Пары, Подключения, Машины, Пользователи, Дрейф, Сертификаты, Синк, API', () => {
    mockFetch(() => undefined);
    const { container } = renderWithProviders(<AppSidebar onLogout={() => {}} authEnabled={false} />, { route: '/' });

    const nav = container.querySelector('.sider-nav') as HTMLElement;
    const items = within(nav)
      .getAllByRole('menuitem')
      .map((el) => el.textContent);
    expect(items).toEqual([
      ru.menu.overview,
      ru.menu.servers,
      ru.menu.map,
      ru.menu.nodes,
      ru.menu.connections,
      ru.menu.users,
      ru.menu.events,
      ru.menu.apiDocs,
    ]);
  });

  it('на «/» выбран «Обзор», на «/connections» - только «Подключения»', () => {
    mockFetch(() => undefined);
    const { container, unmount } = renderWithProviders(<AppSidebar onLogout={() => {}} authEnabled={false} />, { route: '/' });
    const selected = () =>
      Array.from((container.querySelector('.sider-nav') as HTMLElement).querySelectorAll('.ant-menu-item-selected')).map(
        (el) => el.textContent,
      );
    expect(selected()).toEqual([ru.menu.overview]);
    unmount();

    const second = renderWithProviders(<AppSidebar onLogout={() => {}} authEnabled={false} />, { route: '/connections' });
    const nav = second.container.querySelector('.sider-nav') as HTMLElement;
    expect(Array.from(nav.querySelectorAll('.ant-menu-item-selected')).map((el) => el.textContent)).toEqual([ru.menu.connections]);
    expect(screen.getAllByText(ru.menu.overview).length).toBeGreaterThan(0);
  });
});

describe('маршруты', () => {
  const children = routes[0].children ?? [];
  it('индекс - «Обзор», есть карта и события', () => {
    expect(children.find((r) => r.index)).toBeTruthy();
    for (const path of ['servers', 'map', 'connections', 'nodes', 'users', 'events', 'api-docs']) {
      expect(children.some((r) => r.path === path)).toBe(true);
    }
  });
});

describe('formatBytes / formatPct', () => {
  it.each([
    [0, '0 B'],
    [512, '512 B'],
    [1536, '1.5 KB'],
    [1024 * 1024, '1 MB'],
    [5.25 * 1024 ** 3, '5.3 GB'],
    [150 * 1024 ** 2, '150 MB'],
    [-1, '-'],
    [null, '-'],
  ])('%s -> %s', (n, want) => {
    expect(formatBytes(n as number | null)).toBe(want);
  });

  it.each([
    [12.5, '12.5%'],
    [40, '40%'],
    [33.333, '33.3%'],
    [undefined, '-'],
  ])('%s -> %s', (v, want) => {
    expect(formatPct(v as number | undefined)).toBe(want);
  });
});

describe('productName', () => {
  it('mtg показывается как mtg-multi, остальное как есть', async () => {
    const { productName } = await import('@/utils/format');
    expect(productName('mtg')).toBe('mtg-multi');
    expect(productName('telemt')).toBe('telemt');
  });
});
