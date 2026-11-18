import { describe, expect, it } from 'vitest';
import { screen } from '@testing-library/react';

import NotMasterBanner from '@/components/NotMasterBanner';
import { mockFetch, renderWithProviders } from './test-utils';

describe('каркас тестов', () => {
  it('баннер «не мастер» на реплике', async () => {
    mockFetch((url) => (url === '/api/role' ? { body: { role: 'replica', is_master: false } } : undefined));
    renderWithProviders(<NotMasterBanner />);
    expect(await screen.findByText(/не мастер/)).toBeInTheDocument();
  });
});
