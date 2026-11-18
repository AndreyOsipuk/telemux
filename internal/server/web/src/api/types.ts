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

export interface Paging {
  total: number;
  limit: number;
  offset: number;
}

export interface Page<T> {
  data: T[];
  paging: Paging;
}

// ---- пары и дрейф (ручек ещё нет, данные - заглушки, см. mocks.ts) ----

export type MachineRole = 'relay' | 'server' | 'exit';
export type MachineStatus = 'online' | 'degraded' | 'offline' | 'unknown';
export type ApplyResult = 'applied' | 'rejected' | 'rolled_back' | 'failed' | 'pending';

export interface BinaryVersion {
  name: 'telemt' | 'mtg' | 'telemux';
  version: string;
  // Машина в канарейке раскатки - версия новее, чем на остальных.
  canary: boolean;
}

// Машина-цель: что ей велено (desired) и что она применила (applied).
export interface Target {
  code: string;
  name: string;
  role: MachineRole;
  status: MachineStatus;
  desired_rev: number;
  applied_rev: number | null;
  binary: BinaryVersion;
  last_seen_at: string | null;
  // Через какой мост (код машины-выхода) релей получает состояние; null - напрямую.
  via_bridge: string | null;
  last_error: string | null;
}

export interface ApplyLogEntry {
  at: string;
  target: string;
  rev: number;
  result: ApplyResult;
  message: string | null;
}

// Пара: релей, два его сервера, два выхода.
export interface Pair {
  code: string;
  name: string;
  relay: Target;
  servers: Target[];
  exits: Target[];
}

export interface DriftItem {
  target: string;
  name: string;
  pair: string;
  role: MachineRole;
  desired_rev: number;
  applied_rev: number | null;
  // С какого момента желаемое не совпадает с применённым.
  since: string;
  last_error: string | null;
}

export interface DriftResponse extends Page<DriftItem> {
  threshold_seconds: number;
}
