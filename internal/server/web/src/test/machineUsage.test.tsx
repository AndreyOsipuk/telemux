// Роль машины по серверам и серые счётчики у машин без клиентов.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, within } from '@testing-library/react';

import ru from '@/i18n/ru.json';
import ConnectionsPage from '@/pages/connections/ConnectionsPage';
import { fleetProblems } from '@/pages/overview/OverviewPage';
import { idleUsage, machineUsage, offUsage } from '@/utils/machineUsage';
import { NOW_ISO, machine, pageOf, urlPath } from './fleet-fixtures';
import { endpoint, server } from './server-fixtures';
import { mockFetch, renderWithProviders } from './test-utils';

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('machineUsage', () => {
  const servers = [
    server(1, { relay_code: 'pr1', node_code: 'ps1', relay_only: true, endpoints: [endpoint('ee')] }),
    server(3, { relay_code: 'pr3', node_code: 'ps3', relay_only: true, node_retired: true, endpoints: [endpoint('ee')] }),
    server(6, { relay_code: 'pr2', node_code: 'ps1', relay_only: true, endpoints: [endpoint('dd')] }),
    server(9, { relay_code: 'pr9', node_code: 'ps9', relay_only: false, endpoints: [endpoint('ee', { via_relay: false })] }),
    server(10, { state: 'disabled', relay_code: 'pr10', node_code: 'ps10', endpoints: [endpoint('ee')] }),
  ];
  const u = machineUsage(servers);

  it.each([
    ['pr1', 'serving'],
    ['ps1', 'exit'],
    ['ps3', 'retired'],
    ['ps9', 'serving'],
    ['pr10', undefined],
  ])('%s -> %s', (code, want) => {
    expect(u.get(code)).toBe(want);
  });

  it('серыми - счётчики выхода и выведенной; строка - только выведенной и вне серверов', () => {
    expect(idleUsage('exit')).toBe(true);
    expect(idleUsage('serving')).toBe(false);
    expect(idleUsage(undefined)).toBe(false);
    expect(offUsage('retired')).toBe(true);
    expect(offUsage('exit')).toBe(false);
  });

  it('выведенная машина не попадает в проблемы «Обзора»', () => {
    const ms = [machine('ps3', { seen_at: null }), machine('ps4', { seen_at: null })];
    expect(fleetProblems(ms, (c) => u.get(c) === 'retired').map((p) => p.machine.code)).toEqual(['ps4']);
  });
});

describe('ConnectionsPage: машины без клиентов', () => {
  it('выход - серый прочерк вместо 0, выведенная - метка и серая строка', async () => {
    const sum = (m: string) => ({ machine: m, product: 'telemt' as const, connections_total: 0, users_online: 0, seen_at: NOW_ISO(), stale: false, api_ok: true });
    mockFetch((url) => {
      const p = urlPath(url);
      if (p === '/api/v1/connections/summary') return { body: pageOf([sum('pr1'), sum('ps1'), sum('ps3')], url) };
      if (p === '/api/v1/connections') return { body: pageOf([], url) };
      if (p === '/api/v1/servers')
        return {
          body: pageOf(
            [
              server(1, { relay_code: 'pr1', node_code: 'ps1', relay_only: true, endpoints: [endpoint('ee')] }),
              server(3, { relay_code: 'pr1', node_code: 'ps3', node_retired: true, endpoints: [endpoint('ee')] }),
            ],
            url,
          ),
        };
      return undefined;
    });
    renderWithProviders(<ConnectionsPage />);
    const summary = screen.getByTestId('connections-summary');
    const row = async (code: string) => {
      await within(summary).findAllByText(code);
      return summary.querySelector<HTMLElement>(`tr[data-row-key="${code}"]`)!;
    };
    expect(within(await row('pr1')).getByText('0 подкл. · 0 онлайн')).toBeInTheDocument();
    expect(await within(await row('ps1')).findAllByTestId('idle-count')).not.toHaveLength(0);
    const ps3 = await row('ps3');
    expect(await within(ps3).findByText(ru.usage.retired)).toBeInTheDocument();
    expect(ps3).toHaveClass('row-off');
  });
});
