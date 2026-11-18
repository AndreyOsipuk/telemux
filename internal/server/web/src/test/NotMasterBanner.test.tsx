// Тесты баннера «не мастер»: предупреждение на реплике и ссылка на панель мастера.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';

import NotMasterBanner from '@/components/NotMasterBanner';
import ru from '@/i18n/ru.json';
import { mockFetch, renderWithProviders } from './test-utils';

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('NotMasterBanner', () => {
  it('реплика с master_url - ссылка на мастер', async () => {
    const url = 'https://master.example.test:8080';
    mockFetch((u) => (u === '/api/role' ? { body: { role: 'replica', is_master: false, master_url: url } } : undefined));

    renderWithProviders(<NotMasterBanner />);

    const link = await screen.findByRole('link', { name: url });
    expect(link).toHaveAttribute('href', url);
    expect(screen.getByText(/Эта машина - replica, не мастер/)).toBeInTheDocument();
  });

  it('реплика без master_url - текст без ссылки', async () => {
    mockFetch((u) => (u === '/api/role' ? { body: { role: 'replica', is_master: false } } : undefined));

    renderWithProviders(<NotMasterBanner />);

    expect(
      await screen.findByText('Эта машина - replica, не мастер. Просмотр работает, изменения делаются на мастере.'),
    ).toBeInTheDocument();
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
  });

  it('причина роли (reason) показывается описанием', async () => {
    mockFetch((u) =>
      u === '/api/role' ? { body: { role: 'replica', is_master: false, reason: 'мастер недоступен 2 мин' } } : undefined,
    );

    renderWithProviders(<NotMasterBanner />);

    expect(await screen.findByText('мастер недоступен 2 мин')).toBeInTheDocument();
  });

  it('на мастере баннера нет', async () => {
    const fetchFn = mockFetch((u) =>
      u === '/api/role' ? { body: { role: 'master', is_master: true, master_url: 'https://x.example.test' } } : undefined,
    );

    const { container } = renderWithProviders(<NotMasterBanner />);

    await waitFor(() => expect(fetchFn).toHaveBeenCalled());
    // Даём ответу дойти до компонента.
    await new Promise((r) => setTimeout(r, 50));
    expect(container.querySelector('.ant-alert')).toBeNull();
  });

  it('роль не загрузилась - предупреждение «роль не известна» с текстом ошибки', async () => {
    mockFetch((u) => (u === '/api/role' ? { status: 500, body: { error: 'boom' } } : undefined));

    renderWithProviders(<NotMasterBanner />);

    expect(await screen.findByText(ru.role.unknownBanner)).toBeInTheDocument();
    expect(screen.getByText('Ошибка сервера (500). Подробности - в журнале демона.')).toBeInTheDocument();
    expect(screen.queryByText(/boom/)).not.toBeInTheDocument();
  });

  it('роль не загрузилась по сети - текст про сеть', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))));

    renderWithProviders(<NotMasterBanner />);

    expect(await screen.findByText(ru.role.unknownBanner)).toBeInTheDocument();
    expect(screen.getByText(ru.errors.network)).toBeInTheDocument();
  });
});
