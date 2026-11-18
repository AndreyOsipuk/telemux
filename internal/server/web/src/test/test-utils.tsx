// Источник идеи: форк 3x-ui, frontend/src/test/test-utils.tsx (GPL-3.0).
import { vi } from 'vitest';
import type { ReactElement } from 'react';
import { render } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import type { QueryClient } from '@tanstack/react-query';

import { Providers } from '@/App';
import { makeQueryClient } from '@/api/queries';

export function makeTestQueryClient(): QueryClient {
  const qc = makeQueryClient();
  qc.setDefaultOptions({ queries: { retry: false, refetchInterval: false, refetchOnWindowFocus: false } });
  return qc;
}

export function renderWithProviders(ui: ReactElement, opts: { queryClient?: QueryClient; route?: string } = {}) {
  const queryClient = opts.queryClient ?? makeTestQueryClient();
  const utils = render(
    <Providers queryClient={queryClient}>
      <MemoryRouter initialEntries={[opts.route ?? '/']}>{ui}</MemoryRouter>
    </Providers>,
  );
  return { ...utils, queryClient };
}

type Handler = (
  url: string,
  init?: RequestInit,
) => { status?: number; body?: unknown; headers?: Record<string, string> } | undefined;

// Подменяет fetch: handler по URL и методу возвращает статус, JSON-тело и (необязательно) заголовки.
export function mockFetch(handler: Handler) {
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString();
    const res = handler(url, init) ?? { status: 404, body: { code: 'not_found', message: 'Not Found' } };
    const status = res.status ?? 200;
    return new Response(res.body === undefined ? null : JSON.stringify(res.body), {
      status,
      headers: { 'Content-Type': 'application/json', ...res.headers },
    });
  });
  vi.stubGlobal('fetch', fn);
  return fn;
}
