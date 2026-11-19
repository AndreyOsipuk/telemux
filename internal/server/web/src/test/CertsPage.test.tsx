// Тесты «Сертификатов»: домен, машина, tls.web, срок с подсветкой (меньше 14 дней,
// истёк), ещё не выпущен, ошибка выпуска, пагинация, пустой список, 401 и 500.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import type { CertInfo } from '@/api/types';
import { CERT_WARN_DAYS, certState } from '@/components/CertsTable';
import ru from '@/i18n/ru.json';
import CertsPage from '@/pages/certs/CertsPage';
import { cert, pageOf, urlPath } from './fleet-fixtures';
import { mockFetch, renderWithProviders } from './test-utils';

afterEach(() => {
  vi.unstubAllGlobals();
});

const DAY = 86_400_000;
const inDays = (d: number) => new Date(Date.now() + d * DAY + 3_600_000).toISOString();

const CERTS: CertInfo[] = [
  cert('relay-a', 'ok.example.com'),
  cert('relay-a', 'soon.example.com', { not_after: inDays(5) }),
  cert('relay-b', 'old.example.com', { not_after: new Date(Date.now() - DAY).toISOString() }),
  cert('relay-b', 'new.example.com', { not_after: null, issued_at: null, assigned: false, last_error: 'no DNS zone' }),
];

const certRow = (domain: string) => {
  const el = screen.getAllByRole('row').find((r) => r.getAttribute('data-row-key')?.endsWith(`/${domain}`));
  if (!el) throw new Error(`нет строки ${domain}`);
  return el;
};

describe('certState', () => {
  const now = Date.parse('2026-06-01T12:00:00Z');
  it.each([
    { name: 'нет сертификата', na: null, want: 'missing' },
    { name: 'уже истёк', na: '2026-06-01T11:00:00Z', want: 'expired' },
    { name: 'через 13 дней', na: new Date(now + 13 * DAY).toISOString(), want: 'expiring' },
    { name: `ровно через ${CERT_WARN_DAYS} дней`, na: new Date(now + CERT_WARN_DAYS * DAY).toISOString(), want: 'ok' },
    { name: 'через 60 дней', na: new Date(now + 60 * DAY).toISOString(), want: 'ok' },
  ])('$name -> $want', ({ na, want }) => {
    expect(certState({ not_after: na }, now)).toBe(want);
  });
});

describe('CertsPage', () => {
  it('строки, подсветка истекающих и истёкших, ошибка выпуска', async () => {
    const fetchFn = mockFetch((url) => (urlPath(url) === '/api/v1/certs' ? { body: pageOf(CERTS, url) } : undefined));

    renderWithProviders(<CertsPage />);
    await screen.findByText('ok.example.com');

    const ok = certRow('ok.example.com');
    expect(ok).not.toHaveClass('cert-row-warn');
    expect(within(ok).getByText('relay-a')).toBeInTheDocument();
    expect(within(ok).getByText(ru.certs.assigned)).toBeInTheDocument();

    const soon = certRow('soon.example.com');
    expect(soon).toHaveClass('cert-row-warn');
    expect(within(soon).getByText('осталось 5 д')).toBeInTheDocument();

    const old = certRow('old.example.com');
    expect(old).toHaveClass('cert-row-warn');
    expect(within(old).getByText(ru.certs.expired)).toBeInTheDocument();

    const fresh = certRow('new.example.com');
    expect(within(fresh).getByText(ru.certs.noCert)).toBeInTheDocument();
    expect(within(fresh).getByText(ru.certs.notAssigned)).toBeInTheDocument();
    expect(within(fresh).getByText('no DNS zone')).toBeInTheDocument();

    expect(fetchFn.mock.calls.map(([u]) => String(u))).toContain('/api/v1/certs?limit=20&offset=0');
  });

  it('пагинация: вторая страница - offset=20', async () => {
    const many = Array.from({ length: 22 }, (_, i) => cert('relay-a', `d${String(i + 1).padStart(2, '0')}.example.com`));
    const fetchFn = mockFetch((url) => (urlPath(url) === '/api/v1/certs' ? { body: pageOf(many, url) } : undefined));
    const user = userEvent.setup();

    renderWithProviders(<CertsPage />);
    await screen.findByText('d01.example.com');
    await user.click(screen.getByRole('button', { name: 'Страница 2' }));

    expect(await screen.findByText('d21.example.com')).toBeInTheDocument();
    expect(fetchFn.mock.calls.map(([u]) => String(u))).toContain('/api/v1/certs?limit=20&offset=20');
  });

  it('пусто - «Доменов с сертификатами нет»', async () => {
    mockFetch((url) => (urlPath(url) === '/api/v1/certs' ? { body: pageOf([], url) } : undefined));

    renderWithProviders(<CertsPage />);

    expect(await screen.findByText(ru.certs.empty)).toBeInTheDocument();
  });

  it('401 - «сессия истекла»', async () => {
    mockFetch((url) =>
      urlPath(url) === '/api/v1/certs' ? { status: 401, body: { code: 'unauthorized', message: 'unauthorized' } } : undefined,
    );

    renderWithProviders(<CertsPage />);

    expect(await screen.findByText(ru.errors.unauthorized)).toBeInTheDocument();
  });

  it('500 - понятный текст без деталей базы', async () => {
    mockFetch((url) => (urlPath(url) === '/api/v1/certs' ? { status: 500, body: { code: 'internal', message: 'pq: boom' } } : undefined));

    renderWithProviders(<CertsPage />);

    expect(await screen.findByText('Ошибка сервера (500). Подробности - в журнале демона.')).toBeInTheDocument();
    expect(screen.queryByText(/pq: boom/)).not.toBeInTheDocument();
  });

  it('404 - мастер не ведёт сертификаты: спокойное сообщение, не ошибка', async () => {
    mockFetch((url) => (urlPath(url) === '/api/v1/certs' ? { status: 404, body: { code: 'not_found', message: 'not found' } } : undefined));

    renderWithProviders(<CertsPage />);

    expect(await screen.findByText(ru.certs.off)).toBeInTheDocument();
    expect(screen.queryByText('Не удалось загрузить данные')).not.toBeInTheDocument();
  });
});
