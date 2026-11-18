// Тесты страницы документации API: swagger-ui заменён заглушкой (в jsdom его не грузим).
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import ru from '@/i18n/ru.json';
import ApiDocsPage, { OPENAPI_URL } from '@/pages/api-docs/ApiDocsPage';
import { mockFetch, renderWithProviders } from './test-utils';

vi.mock('swagger-ui-react', () => ({
  default: ({ spec }: { spec: { info?: { title?: string } } }) => (
    <div data-testid="swagger-ui">{spec?.info?.title}</div>
  ),
}));
vi.mock('swagger-ui-react/swagger-ui.css', () => ({}));

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('ApiDocsPage', () => {
  it('спеки нет (404) - понятное сообщение и подсказка с адресом', async () => {
    mockFetch(() => ({ status: 404, body: { error: 'not found' } }));

    renderWithProviders(<ApiDocsPage />);

    expect(await screen.findByText(ru.apiDocs.missingTitle)).toBeInTheDocument();
    expect(screen.getByText(new RegExp(OPENAPI_URL.replace(/\./g, '\\.')))).toBeInTheDocument();
    expect(screen.queryByTestId('swagger-ui')).not.toBeInTheDocument();
  });

  it('500 - «не удалось загрузить» с текстом ошибки сервера', async () => {
    mockFetch(() => ({ status: 500, body: { error: 'boom' } }));

    renderWithProviders(<ApiDocsPage />);

    expect(await screen.findByText(ru.apiDocs.failedTitle)).toBeInTheDocument();
    expect(screen.getByText('Ошибка сервера (500). Подробности - в журнале демона.')).toBeInTheDocument();
  });

  it('спека есть - отдаётся в swagger-ui', async () => {
    const fetchFn = mockFetch((url) =>
      url === OPENAPI_URL ? { body: { openapi: '3.1.0', info: { title: 'telemux API' } } } : undefined,
    );

    renderWithProviders(<ApiDocsPage />);

    expect(await screen.findByTestId('swagger-ui')).toHaveTextContent('telemux API');
    expect(fetchFn).toHaveBeenCalledWith(OPENAPI_URL, expect.anything());
  });

  it('«Повторить» после 404 перезапрашивает спеку и показывает её', async () => {
    let published = false;
    mockFetch((url) => {
      if (url !== OPENAPI_URL) return undefined;
      return published ? { body: { info: { title: 'telemux API' } } } : { status: 404, body: {} };
    });
    const user = userEvent.setup();

    renderWithProviders(<ApiDocsPage />);
    await screen.findByText(ru.apiDocs.missingTitle);

    published = true;
    await user.click(screen.getByRole('button', { name: ru.common.retry }));

    expect(await screen.findByTestId('swagger-ui')).toHaveTextContent('telemux API');
  });
});
