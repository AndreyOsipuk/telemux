// Источник: форк 3x-ui, frontend/src/pages/nodes/NodeList.tsx, StatusDot/StatusLabel
// (GPL-3.0). Адаптировано под статусы машин telemux.

import { Badge, Space } from 'antd';
import type { BadgeProps } from 'antd';
import { useTranslation } from 'react-i18next';

import type { MachineStatus } from '@/api/types';
import './StatusDot.css';

const BADGE: Record<MachineStatus, BadgeProps['status']> = {
  online: 'success',
  degraded: 'warning',
  offline: 'error',
  unknown: 'default',
};

export function StatusDot({ status }: { status: MachineStatus }) {
  if (status === 'online') return <span className="online-dot" aria-hidden />;
  return <Badge status={BADGE[status]} />;
}

export default function StatusCell({ status }: { status: MachineStatus }) {
  const { t } = useTranslation();
  const color =
    status === 'online'
      ? 'var(--ant-color-success)'
      : status === 'degraded'
        ? 'var(--ant-color-warning)'
        : status === 'offline'
          ? 'var(--ant-color-error)'
          : undefined;
  return (
    <Space size={6}>
      <StatusDot status={status} />
      <span style={{ color }}>{t(`status.${status}`)}</span>
    </Space>
  );
}
