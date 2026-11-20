// Путь «релей - своя нода» (спецификация relay-via-exit, R3.5): статусы пар из
// /api/v1/pairs в таблице машин и новые виды событий в ленте.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';

import type { Pair } from '@/api/types';
import ru from '@/i18n/ru.json';
import { RelayPathCell, relayPaths } from '@/components/RelayPath';
import { EventKindTag } from '@/pages/events/EventsPage';
import { machine, pairMachine } from './fleet-fixtures';
import { renderWithProviders } from './test-utils';

afterEach(() => {
  vi.unstubAllGlobals();
});

const seen = '2026-11-20T10:00:00Z';

const pairs: Pair[] = [
  {
    relay: pairMachine('pr2', { kind: 'relay', relay_via: 'ps9', relay_via_at: seen, foreign_bridge: true }),
    exits: [
      pairMachine('ps2', { relay_code: 'pr2', relay_status: 'ok', relay_last_seen: seen }),
      pairMachine('ps1', { relay_code: 'pr2', relay_status: 'relay_silent', relay_last_seen: null }),
      pairMachine('ps3', { relay_code: 'pr2', relay_status: 'exit_down', relay_last_seen: null }),
    ],
    relay_down: true,
    foreign_bridge: true,
    status: 'down',
  },
  { relay: pairMachine('pr4', { kind: 'relay' }), exits: [], relay_down: false, foreign_bridge: false, status: 'no_exit' },
  { relay: null, exits: [pairMachine('panel1', { kind: 'panel' })] },
];

describe('relayPaths R3.5', () => {
  it('статус выхода и статус пары по коду машины', () => {
    const m = relayPaths(pairs);
    expect(m.get('ps1')).toMatchObject({ relayStatus: 'relay_silent' });
    expect(m.get('ps3')).toMatchObject({ relayStatus: 'exit_down' });
    expect(m.get('pr2')).toMatchObject({ pairStatus: 'down', foreignBridge: true });
    expect(m.get('pr4')).toMatchObject({ pairStatus: 'no_exit' });
    expect(m.has('panel1')).toBe(false);
  });
});

describe('RelayPathCell R3.5', () => {
  const paths = relayPaths(pairs);

  it('выход: релей и статус пары', () => {
    renderWithProviders(<RelayPathCell m={machine('ps1', { relay_code: 'pr2' })} path={paths.get('ps1')} />);
    expect(screen.getByText('pr2')).toBeInTheDocument();
    expect(screen.getByText(ru.fleet.relayStatus.relay_silent)).toBeInTheDocument();
  });

  it('релей: упал, чужой мост и через чей мост ходит', () => {
    renderWithProviders(<RelayPathCell m={machine('pr2', { kind: 'relay', relay_via: 'ps9' })} path={paths.get('pr2')} />);
    expect(screen.getByText(ru.fleet.pairStatus.down)).toBeInTheDocument();
    expect(screen.getByText(ru.fleet.foreignBridge)).toBeInTheDocument();
    expect(screen.getByText('ps9')).toBeInTheDocument();
  });

  it('релей без своих нод', () => {
    renderWithProviders(<RelayPathCell m={machine('pr4', { kind: 'relay' })} path={paths.get('pr4')} />);
    expect(screen.getByText(ru.fleet.pairStatus.no_exit)).toBeInTheDocument();
  });

  it('пар ещё нет - просто код релея', () => {
    renderWithProviders(<RelayPathCell m={machine('ps5', { relay_code: 'pr5' })} path={undefined} />);
    expect(screen.getByText('pr5')).toBeInTheDocument();
  });
});

describe('EventKindTag R4.1', () => {
  it.each(['relay_silent', 'relay_ok', 'foreign_bridge', 'relay_no_exit'] as const)('%s подписан', (kind) => {
    renderWithProviders(<EventKindTag kind={kind} />);
    expect(screen.getByText(ru.events.kinds[kind])).toBeInTheDocument();
  });
});
