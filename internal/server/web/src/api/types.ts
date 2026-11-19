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
