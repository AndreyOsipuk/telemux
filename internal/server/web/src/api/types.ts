// Типы API панели. То, что уже описано в api/openapi.yaml, берётся из
// сгенерированного generated/openapi.ts (npm run gen:api). Остальное - временные
// типы, пока старые ручки (/api/users, /api/nodes, ...) не попали в спеку.

import type { components } from './generated/openapi';

export type ApiErrorBody = components['schemas']['Error'];
export type ErrorCode = ApiErrorBody['code'];
export type RevisionTargetStatus = components['schemas']['TargetStatus'];

// ---- то, что бэкенд отдаёт уже сейчас ----

export interface Me {
  auth_enabled: boolean;
  authed: boolean;
}

export interface Role {
  role: string;
  is_master: boolean;
  // Поля автомата роли (ADR 0002); у старых демонов их нет.
  reason?: string;
  master?: string;
  master_url?: string;
}

export interface SyncStatus {
  at: string;
  mode: string;
  creates: number;
  patches: number;
  deletes: number;
  applied: number;
  failed: number;
  aborted: boolean;
  error?: string;
}

export interface ClusterNode {
  code: string;
  name: string;
  address: string;
  telemt_api_url: string;
  role: string;
  enabled: boolean;
  last_seen_at: string | null;
}

export interface User {
  username: string;
  secret: string;
  expiration_at: string | null;
  max_tcp_conns: number | null;
  enabled: boolean;
  created_at: string;
}

export interface NewUser {
  username: string;
  expiration_at?: string | null;
  max_tcp_conns?: number | null;
}

// ---- флот: машины, пары, дрейф, журнал применений (api/openapi.yaml) ----

export type Paging = components['schemas']['Paging'];

export interface Page<T> {
  data: T[];
  paging: Paging;
}

export type Machine = components['schemas']['Machine'];
export type MachineKind = Machine['kind'];
export type ObservedCheck = components['schemas']['ObservedCheck'];
export type ObservedResource = Machine['observed'][number];
export type PairMachine = components['schemas']['PairMachine'];
// Сводный статус машины в паре: ok, behind, failed, silent, no_agent, disabled.
export type PairStatus = PairMachine['status'];
export type DesiredResource = PairMachine['desired'][number];
// Статус своей ноды релея в паре (relay-via-exit, R3.2): ok, relay_silent, exit_down.
export type RelayStatus = NonNullable<PairMachine['relay_status']>;
// Статус пары: ok, down (релей упал по R3.3), no_exit (у релея нет своих нод).
export type PairPathStatus = NonNullable<components['schemas']['Pair']['status']>;
// relay: null - псевдо-пара «без релея».
export type Pair = components['schemas']['Pair'];
export type DriftItem = components['schemas']['DriftItem'];
export type DriftReason = DriftItem['reason'];
export type ApplyLogEntry = components['schemas']['ApplyLogEntry'];

// Добавление машины и команда установки агента (createTarget, createJoin).
export type NewTarget = components['schemas']['NewTarget'];
export type NewTargetKind = NewTarget['kind'];
export type TargetResource = NonNullable<NewTarget['resources']>[number];
export type JoinCommand = components['schemas']['JoinCommand'];
// opened, not_configured, failed, not_needed; у старого мастера поля нет.
export type FirewallStatus = NonNullable<JoinCommand['firewall']>;

export interface DriftResponse extends Page<DriftItem> {
  threshold_seconds: number;
}

// Статус управляющего узла кластера (/api/nodes) по последнему heartbeat.
export type MachineStatus = 'online' | 'degraded' | 'offline' | 'unknown';

// ---- runtime-отчёты агентов: нагрузка, подключения, журнал, сертификаты ----

// Сводный runtime-статус машины (считает мастер): online, stale, degraded, offline.
export type RuntimeStatus = NonNullable<Machine['runtime_status']>;
export type RuntimeHost = components['schemas']['RuntimeHost'];
export type MachineRuntime = components['schemas']['MachineRuntime'];
export type Product = components['schemas']['Product'];
export type Connection = components['schemas']['Connection'];
export type ConnectionSummary = components['schemas']['ConnectionSummary'];
export type LogErrorSum = components['schemas']['LogErrorSum'];
export type LogErrorKind = components['schemas']['LogErrorKind'];
export type CertInfo = components['schemas']['CertInfo'];
export type UserConnections = components['schemas']['UserConnections'];
export type Overview = components['schemas']['Overview'];

// Фильтры списка подключений; пустые поля не передаются.
export interface ConnFilter {
  machine?: string;
  product?: Product;
  minConnections?: number;
  user?: string;
}

// ---- серверы: то, что видит клиент (docs/adr/0005-servers.md) ----
// Типы написаны вручную по контракту ADR; после перегенерации openapi.ts их можно
// брать из components['schemas'].

export type ServerEngine = 'telemt' | 'mtg';
// maintenance - клиент видит «на профилактике»; test - скрыт от клиентов; disabled - выключен везде.
export type ServerState = 'active' | 'maintenance' | 'test' | 'disabled';
export type EndpointProtocol = 'ee' | 'dd' | 'web';

// Точка входа - одна ссылка клиента.
export interface ServerEndpoint {
  id?: number;
  protocol: EndpointProtocol;
  host: string;
  // Для web не используется.
  port: number;
  // Домен в ee-секрете; для ee обязателен.
  tls_domain: string;
  via_relay: boolean;
  label: string;
  sort_order: number;
  enabled: boolean;
  note: string;
}

// Живые цифры машины сервера по последнему runtime-отчёту агента.
export interface ServerMachineLive {
  code: string;
  seen_at: string | null;
  connections: number;
  users_online: number;
}

export interface Server {
  no: number;
  name: string;
  emoji: string;
  country: string;
  engine: ServerEngine;
  relay_code: string | null;
  node_code: string | null;
  state: ServerState;
  relay_only: boolean;
  node_retired: boolean;
  tls_domain: string;
  extra_tls_domains: string[];
  // null - общий лимит по умолчанию.
  user_max_tcp_conns: number | null;
  // null - без лимита.
  user_max_unique_ips: number | null;
  // Настройки движка по каталогу полей; ключа нет - значение по умолчанию.
  settings: Record<string, string>;
  note: string;
  endpoints: ServerEndpoint[];
  created_at: string;
  updated_at: string;
  live: { relay: ServerMachineLive | null; node: ServerMachineLive | null };
  // Кто публикует конфиги машин сервера (ADR 0006); у старого мастера поля нет.
  config_source?: ConfigSource;
  // Сводка последнего сравнения сборки telemux с текущими конфигами.
  render?: ServerRenderSummary;
}

// Тело POST /api/v1/servers и PUT /api/v1/servers/{no}: источник конфига меняется
// отдельной ручкой, сводка сборки - только для чтения.
export type ServerInput = Omit<Server, 'live' | 'created_at' | 'updated_at' | 'config_source' | 'render'>;

// ---- сборка конфигов telemux (ADR 0006) ----
export type ConfigSource = components['schemas']['ConfigSource'];
export type ServerRenderSummary = components['schemas']['ServerRenderSummary'];
export type ServerRenderItem = components['schemas']['ServerRenderItem'];

// ---- машины как ноды 3x-ui (ADR 0007) ----
export type TargetPatch = components['schemas']['TargetPatch'];
export type Target = components['schemas']['Target'];
export type MachineCheck = components['schemas']['MachineCheck'];
export type EndpointCheck = components['schemas']['EndpointCheck'];
export type MachineMetrics = components['schemas']['MachineMetrics'];
export type MetricPoint = components['schemas']['MetricPoint'];
export type MetricStep = MachineMetrics['step'];
export type MachineEvent = components['schemas']['MachineEvent'];
export type MachineEventKind = MachineEvent['kind'];

export type ServerFieldType = 'int' | 'bool' | 'string' | 'enum' | 'hostport' | 'host' | 'duration';
// instant - без касания машин; reload - перечитывание конфига; restart - рвёт подключения; firewall - iptables.
export type ServerFieldApply = 'instant' | 'reload' | 'restart' | 'firewall';

// Поле каталога настроек движка (GET /api/v1/servers/fields).
export interface ServerField {
  key: string;
  engine: ServerEngine;
  group: string;
  type: ServerFieldType;
  default: string;
  min?: number;
  max?: number;
  enum?: string[];
  apply: ServerFieldApply;
  // Смена меняет ссылки всех пользователей или рвёт подключения.
  danger: boolean;
  title_ru: string;
  title_en: string;
  hint_ru: string;
  hint_en: string;
}

// Фильтры списка серверов; пустые поля не передаются.
export interface ServerFilter {
  engine?: ServerEngine;
  state?: ServerState;
}

// ---- версии telemt и mtg-multi, обновление из релизов GitHub (ADR 0008) ----
export type StackStatus = components['schemas']['StackStatus'];
export type StackProduct = components['schemas']['StackProduct'];
export type StackMachine = components['schemas']['StackMachine'];
export type StackProductState = components['schemas']['StackProductState'];
export type StackUpdateState = components['schemas']['StackUpdateState'];
export type StackUpdateRequest = components['schemas']['StackUpdateRequest'];
