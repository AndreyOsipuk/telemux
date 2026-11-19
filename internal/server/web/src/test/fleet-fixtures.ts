// Данные флота по схемам api/openapi.yaml (Machine, PairMachine, Pair, DriftItem,
// ApplyLogEntry) и бэкенд-обработчик постраничных списков для mockFetch.
import type {
  ApplyLogEntry,
  CertInfo,
  Connection,
  DriftItem,
  Machine,
  MachineRuntime,
  Overview,
  Page,
  Pair,
  PairMachine,
} from '@/api/types';

export const NOW_ISO = () => new Date(Date.now() - 30_000).toISOString();

export function machine(code: string, over: Partial<Machine> = {}): Machine {
  return {
    code,
    kind: 'exit',
    enabled: true,
    resources: ['telemt.users'],
    agent_version: '0.6.0',
    seen_at: NOW_ISO(),
    checks: [],
    observed: [{ resource: 'telemt.users', revision: 7, status: 'applied', seen_at: NOW_ISO() }],
    runtime_seen_at: null,
    host: null,
    telemt_api_ok: null,
    mtg_api_ok: null,
    connections: 0,
    users_online: 0,
    ...over,
  };
}

// Машина со свежим runtime-отчётом: нагрузка, версии, подключения, статус online.
export function runtimeMachine(code: string, over: Partial<Machine> = {}): Machine {
  return machine(code, {
    runtime_seen_at: NOW_ISO(),
    host: { cpu_pct: 12.5, mem_pct: 40, disk_pct: 55, load1: 0.42, net_rx_mbps: 10, net_tx_mbps: 11, uptime_s: 3 * 86400 },
    telemt_api_ok: true,
    telemt_version: '3.5.17',
    connections: 40,
    users_online: 6,
    runtime_status: 'online',
    ...over,
  });
}

export function pairMachine(code: string, over: Partial<PairMachine> = {}): PairMachine {
  return {
    ...machine(code),
    status: 'ok',
    desired: [{ resource: 'telemt.users', revision: 7 }],
    ...over,
  };
}

export function driftItem(target: string, over: Partial<DriftItem> = {}): DriftItem {
  return {
    target,
    kind: 'exit',
    reason: 'behind',
    resource: 'telemt.users',
    desired_revision: 8,
    observed_revision: 7,
    since: new Date(Date.now() - 3_600_000).toISOString(),
    seen_at: NOW_ISO(),
    ...over,
  };
}

export function logEntry(i: number, over: Partial<ApplyLogEntry> = {}): ApplyLogEntry {
  return {
    at: new Date(Date.UTC(2026, 10, 1, 12, 0, 0) - i * 60_000).toISOString(),
    resource: 'telemt.users',
    revision: 100 - i,
    status: 'applied',
    ...over,
  };
}

// Страница {data, paging} по limit/offset из URL.
export function pageOf<T>(all: T[], url: string): Page<T> {
  const q = new URL(url, 'http://x').searchParams;
  const limit = Number(q.get('limit') ?? 20);
  const offset = Number(q.get('offset') ?? 0);
  return { data: all.slice(offset, offset + limit), paging: { total: all.length, limit, offset } };
}

export const urlPath = (url: string) => new URL(url, 'http://x').pathname;
export const urlParam = (url: string, name: string) => new URL(url, 'http://x').searchParams.get(name);

export type { Pair };

// Ответ /api/v1/overview: итоги по умолчанию считаются по machines.
export function overviewOf(machines: Machine[], over: Partial<Overview> = {}): Overview {
  return {
    machines: {
      total: machines.length,
      online: machines.filter((m) => m.runtime_status === 'online' || m.runtime_status === 'degraded').length,
      silent: machines.filter((m) => m.runtime_status === 'offline').length,
    },
    connections: { total: machines.reduce((n, m) => n + m.connections, 0), users_online: 9 },
    pairs: { total: 3, ok: 2, problem: 1 },
    certs: { total: 4, expiring: 1, missing: 1 },
    cert_warn_days: 14,
    stale_after_seconds: 180,
    silent_after_seconds: 600,
    data: machines,
    truncated: false,
    ...over,
  };
}

export function machineRuntime(code: string, over: Partial<MachineRuntime> = {}): MachineRuntime {
  return {
    code,
    seen_at: NOW_ISO(),
    stale: false,
    agent_version: '0.7.0',
    host: { cpu_pct: 12.5, mem_pct: 40, disk_pct: 91, load1: 0.42, net_rx_mbps: 10.25, net_tx_mbps: 11, uptime_s: 3 * 86400 },
    telemt: {
      version: '3.5.17',
      api_ok: true,
      error: '',
      users_configured: 120,
      users_effective: 118,
      connections_total: 340,
      tls_domain: 'cdn.example.org',
      max_tcp_conns_median: 8,
      config_sha256: 'abcdef0123456789',
    },
    mtg: { version: '2.1.7', api_ok: false, error: 'connection refused', connections_total: 0 },
    ...over,
  };
}

export function connection(machine: string, key: string, over: Partial<Connection> = {}): Connection {
  return {
    machine,
    product: 'telemt',
    key,
    connections: 2,
    active_ips: ['192.0.2.10', '198.51.100.7'],
    bytes_in: 0,
    bytes_out: 0,
    last_seen: null,
    seen_at: NOW_ISO(),
    stale: false,
    ...over,
  };
}

export function cert(machine: string, domain: string, over: Partial<CertInfo> = {}): CertInfo {
  return {
    machine,
    domain,
    assigned: true,
    not_after: new Date(Date.now() + 60 * 86_400_000).toISOString(),
    issued_at: new Date(Date.now() - 30 * 86_400_000).toISOString(),
    last_try_at: new Date(Date.now() - 3_600_000).toISOString(),
    ...over,
  };
}
