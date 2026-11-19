// Источник: форк 3x-ui, frontend/src/pages/nodes/NodeList.tsx, StatusDot/StatusLabel
// (GPL-3.0). Адаптировано под статусы telemux: управляющие узлы кластера
// (online/degraded/offline/unknown) и машины флота в паре (ok/behind/failed/...).

import { Badge, Space } from 'antd';
import type { BadgeProps } from 'antd';
import { useTranslation } from 'react-i18next';

import type { MachineStatus, PairStatus } from '@/api/types';
import './StatusDot.css';

export type DotStatus = MachineStatus | PairStatus;

const BADGE: Record<DotStatus, BadgeProps['status']> = {
  online: 'success',
  degraded: 'warning',
  offline: 'error',
  unknown: 'default',
  ok: 'success',
  behind: 'processing',
  failed: 'error',
  silent: 'warning',
  no_agent: 'default',
  disabled: 'default',
};

const COLOR: Partial<Record<DotStatus, string>> = {
  online: 'var(--ant-color-success)',
  ok: 'var(--ant-color-success)',
  degraded: 'var(--ant-color-warning)',
  silent: 'var(--ant-color-warning)',
  behind: 'var(--ant-color-primary)',
  offline: 'var(--ant-color-error)',
  failed: 'var(--ant-color-error)',
};

export function StatusDot({ status }: { status: DotStatus }) {
  if (status === 'online' || status === 'ok') return <span className="online-dot" aria-hidden />;
  return <Badge status={BADGE[status]} />;
}

export default function StatusCell({ status }: { status: DotStatus }) {
  const { t } = useTranslation();
  return (
    <Space size={6} data-status={status}>
      <StatusDot status={status} />
      <span style={{ color: COLOR[status] }}>{t(`status.${status}`)}</span>
    </Space>
  );
}
