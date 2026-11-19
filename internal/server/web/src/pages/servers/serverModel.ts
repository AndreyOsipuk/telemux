// Преобразования сервера между API (ServerInput) и формой панели, превью ссылок,
// список того, что изменится при сохранении, и свежесть машин сервера.

import type {
  EndpointProtocol,
  Server,
  ServerEndpoint,
  ServerEngine,
  ServerField,
  ServerInput,
  ServerMachineLive,
  ServerState,
} from '@/api/types';
import type { DotStatus } from '@/components/StatusDot';

export const ENGINES: ServerEngine[] = ['telemt', 'mtg'];
export const STATES: ServerState[] = ['active', 'maintenance', 'test', 'disabled'];
export const PROTOCOLS: EndpointProtocol[] = ['ee', 'dd', 'web'];

export const DEFAULT_PORT = 443;
export const DEFAULT_SORT = 100;

// Значение настройки в форме: число (int), флаг (bool) или строка; пусто - по умолчанию.
export type SettingValue = string | number | boolean | null | undefined;

export interface EndpointValues {
  id?: number;
  protocol: EndpointProtocol;
  host?: string;
  port?: number | null;
  tls_domain?: string;
  via_relay: boolean;
  label?: string;
  sort_order?: number | null;
  enabled: boolean;
  note?: string;
}

export interface ServerFormValues {
  no?: number | null;
  name?: string;
  emoji?: string;
  country?: string;
  engine: ServerEngine;
  relay_code?: string | null;
  node_code?: string | null;
  state: ServerState;
  relay_only: boolean;
  node_retired: boolean;
  note?: string;
  endpoints: EndpointValues[];
  tls_domain?: string;
  extra_tls_domains: string[];
  user_max_tcp_conns?: number | null;
  user_max_unique_ips?: number | null;
  settings: Record<string, SettingValue>;
}

const TRUE_VALUES = new Set(['1', 'true', 'yes', 'on']);
export const isTrue = (v: string | undefined) => TRUE_VALUES.has((v ?? '').trim().toLowerCase());

export function newEndpoint(protocol: EndpointProtocol = 'ee', tlsDomain = ''): EndpointValues {
  return {
    protocol,
    host: '',
    port: DEFAULT_PORT,
    tls_domain: protocol === 'ee' ? tlsDomain : '',
    via_relay: true,
    label: '',
    sort_order: DEFAULT_SORT,
    enabled: true,
    note: '',
  };
}

export function emptyFormValues(): ServerFormValues {
  return {
    engine: 'telemt',
    state: 'active',
    relay_only: false,
    node_retired: false,
    endpoints: [],
    extra_tls_domains: [],
    settings: {},
  };
}

// Строка настройки из API -> значение контрола формы по типу поля.
export function settingToForm(field: ServerField, raw: string | undefined): SettingValue {
  if (raw === undefined) return undefined;
  if (field.type === 'bool') return isTrue(raw);
  if (field.type === 'int') {
    const n = Number(raw);
    return Number.isFinite(n) ? n : raw;
  }
  return raw;
}

// Значение контрола -> строка для API; undefined - ключ не пишется (значение по умолчанию).
export function settingFromForm(field: ServerField, v: SettingValue): string | undefined {
  if (v === undefined || v === null) return undefined;
  if (field.type === 'bool') return v ? '1' : '0';
  const s = String(v).trim();
  return s === '' ? undefined : s;
}

export function toFormValues(s: Server, fields: ServerField[]): ServerFormValues {
  const settings: Record<string, SettingValue> = {};
  for (const f of fields) {
    if (f.engine === s.engine && s.settings[f.key] !== undefined) settings[f.key] = settingToForm(f, s.settings[f.key]);
  }
  return {
    no: s.no,
    name: s.name,
    emoji: s.emoji,
    country: s.country,
    engine: s.engine,
    relay_code: s.relay_code,
    node_code: s.node_code,
    state: s.state,
    relay_only: s.relay_only,
    node_retired: s.node_retired,
    note: s.note,
    endpoints: [...s.endpoints]
      .sort((a, b) => a.sort_order - b.sort_order)
      .map((e) => ({ ...e })),
    tls_domain: s.tls_domain,
    extra_tls_domains: [...s.extra_tls_domains],
    user_max_tcp_conns: s.user_max_tcp_conns,
    user_max_unique_ips: s.user_max_unique_ips,
    settings,
  };
}

const str = (v: string | null | undefined) => (v ?? '').trim();
const numOrNull = (v: number | null | undefined) => (v === undefined || v === null ? null : v);

function toEndpoint(e: EndpointValues): ServerEndpoint {
  const out: ServerEndpoint = {
    protocol: e.protocol,
    host: str(e.host),
    port: e.port ?? DEFAULT_PORT,
    tls_domain: e.protocol === 'ee' ? str(e.tls_domain) : '',
    via_relay: e.via_relay,
    label: str(e.label),
    sort_order: e.sort_order ?? DEFAULT_SORT,
    enabled: e.enabled,
    note: str(e.note),
  };
  if (e.id !== undefined) out.id = e.id;
  return out;
}

// Тело POST/PUT. Настройки движка: значения из каталога текущего движка, пустые не
// пишутся; ключи, которых каталог не знает (или он не загрузился), остаются как были.
export function toServerInput(
  v: ServerFormValues,
  fields: ServerField[],
  original: Record<string, string> = {},
): ServerInput {
  const catalog = fields.filter((f) => f.engine === v.engine);
  const known = new Set(fields.map((f) => f.key));
  const settings: Record<string, string> = {};
  for (const [k, val] of Object.entries(original)) if (!known.has(k)) settings[k] = val;
  for (const f of catalog) {
    const s = settingFromForm(f, v.settings?.[f.key]);
    if (s !== undefined) settings[f.key] = s;
  }
  return {
    no: v.no ?? 0,
    name: str(v.name),
    emoji: str(v.emoji),
    country: str(v.country).toLowerCase(), // бэкенд: ^[a-z]{2}$ или пусто
    engine: v.engine,
    relay_code: v.relay_code || null,
    node_code: v.node_code || null,
    state: v.state,
    relay_only: v.relay_only,
    node_retired: v.node_retired,
    tls_domain: str(v.tls_domain),
    extra_tls_domains: (v.extra_tls_domains ?? []).map((d) => d.trim()).filter(Boolean),
    user_max_tcp_conns: numOrNull(v.user_max_tcp_conns),
    user_max_unique_ips: numOrNull(v.user_max_unique_ips),
    settings,
    note: str(v.note),
    endpoints: (v.endpoints ?? []).map(toEndpoint),
  };
}

// Превью ссылки без секрета (секрет заменён на «…») в формате бота: ee и dd -
// tg://proxy с портом, web - tg://webproxy без порта, секрет dd.
export function linkPreview(e: Pick<EndpointValues, 'protocol' | 'host' | 'port'>): string {
  const host = str(e.host) || '…';
  if (e.protocol === 'web') return `tg://webproxy?server=${host}&secret=dd…`;
  return `tg://proxy?server=${host}&port=${e.port ?? DEFAULT_PORT}&secret=${e.protocol}…`;
}

// Что изменится для пользователей при сохранении правки (для подтверждения).
export type Change =
  | { kind: 'danger'; key: string; from: string; to: string }
  | { kind: 'tlsDomain'; from: string; to: string }
  | { kind: 'eeEndpoints' }
  | { kind: 'disable' };

const eeSig = (e: ServerEndpoint) => `${e.host}|${e.port}|${e.tls_domain}|${e.via_relay}|${e.enabled}`;

export function riskyChanges(before: ServerInput, after: ServerInput, fields: ServerField[]): Change[] {
  const out: Change[] = [];
  for (const f of fields) {
    if (!f.danger || f.engine !== after.engine) continue;
    const from = before.settings[f.key] ?? '';
    const to = after.settings[f.key] ?? '';
    if (from !== to) out.push({ kind: 'danger', key: f.key, from: from || f.default, to: to || f.default });
  }
  if (before.tls_domain !== after.tls_domain) out.push({ kind: 'tlsDomain', from: before.tls_domain, to: after.tls_domain });
  // ee-точка удалена или изменена: каждой прежней ee-точке нужна такая же в новом списке.
  const afterEe = new Set(after.endpoints.filter((e) => e.protocol === 'ee').map(eeSig));
  if (before.endpoints.some((e) => e.protocol === 'ee' && !afterEe.has(eeSig(e)))) out.push({ kind: 'eeEndpoints' });
  if (before.state !== 'disabled' && after.state === 'disabled') out.push({ kind: 'disable' });
  return out;
}

// Свежесть машины сервера по seen_at runtime-отчёта (пороги - как у мастера по умолчанию).
export const STALE_AFTER_S = 180;
export const SILENT_AFTER_S = 600;

export function liveStatus(m: ServerMachineLive, now: number): DotStatus {
  if (!m.seen_at) return 'unknown';
  const ts = Date.parse(m.seen_at);
  if (Number.isNaN(ts)) return 'unknown';
  const age = (now - ts) / 1000;
  if (age < STALE_AFTER_S) return 'online';
  if (age < SILENT_AFTER_S) return 'stale';
  return 'offline';
}

const SEVERITY: Partial<Record<DotStatus, number>> = { online: 0, unknown: 1, stale: 2, offline: 3 };

// Худший статус среди машин сервера: одна молчит - сервер виден как проблемный.
export function worstStatus(statuses: DotStatus[]): DotStatus | null {
  if (statuses.length === 0) return null;
  return statuses.reduce((a, b) => ((SEVERITY[b] ?? 0) > (SEVERITY[a] ?? 0) ? b : a));
}
