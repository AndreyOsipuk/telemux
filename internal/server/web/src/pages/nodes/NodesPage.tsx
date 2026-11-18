// Машины кластера (/api/nodes): роль, адрес (скрыт по умолчанию, как в 3x-ui),
// статус по последнему heartbeat и «видели N секунд назад».

import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button, Card, Table, Tag, Tooltip } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { EyeInvisibleOutlined, EyeOutlined, ReloadOutlined } from '@ant-design/icons';

import { useNodes } from '@/api/queries';
import type { ClusterNode, MachineStatus } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import StatusCell from '@/components/StatusDot';
import { secondsSince, useNow, useRelativeTime } from '@/hooks/useRelativeTime';
import { formatDateTime } from '@/utils/format';
import './NodesPage.css';

// Heartbeat агента - раз в несколько секунд; дольше этого молчания - машина не на связи.
export const NODE_STALE_SECONDS = 90;

export function nodeStatus(n: ClusterNode, now: number): MachineStatus {
  const age = secondsSince(n.last_seen_at, now);
  if (age === null) return 'unknown';
  return age <= NODE_STALE_SECONDS ? 'online' : 'offline';
}

export default function NodesPage() {
  const { t } = useTranslation();
  const nodes = useNodes();
  const now = useNow();
  const relative = useRelativeTime();
  const [showAddress, setShowAddress] = useState(false);

  const columns = useMemo<ColumnsType<ClusterNode>>(
    () => [
      {
        title: t('nodes.code'),
        dataIndex: 'code',
        render: (v: string, n) => (
          <div className="name-cell">
            <span className="name">{v}</span>
            {n.name && n.name !== v && <span className="remark">{n.name}</span>}
          </div>
        ),
      },
      {
        title: t('nodes.role'),
        dataIndex: 'role',
        render: (v: string) => <Tag color={v === 'master' ? 'blue' : 'default'}>{v}</Tag>,
      },
      {
        title: (
          <span className="address-header">
            {t('nodes.address')}
            <Tooltip title={t('nodes.toggleAddress')}>
              <Button
                type="text"
                size="small"
                aria-label={t('nodes.toggleAddress')}
                icon={showAddress ? <EyeOutlined /> : <EyeInvisibleOutlined />}
                onClick={() => setShowAddress((v) => !v)}
              />
            </Tooltip>
          </span>
        ),
        dataIndex: 'address',
        ellipsis: true,
        render: (v: string) => <span className={`mono ${showAddress ? 'address-visible' : 'address-hidden'}`}>{v || '-'}</span>,
      },
      {
        title: t('nodes.status'),
        render: (_v, n) => (n.enabled ? <StatusCell status={nodeStatus(n, now)} /> : <Tag>{t('nodes.disabled')}</Tag>),
      },
      {
        title: t('nodes.lastSeen'),
        dataIndex: 'last_seen_at',
        render: (v: string | null) => (
          <Tooltip title={v ? formatDateTime(v) : undefined}>{relative(secondsSince(v, now))}</Tooltip>
        ),
      },
    ],
    [t, showAddress, now, relative],
  );

  return (
    <>
      <div className="page-header">
        <h1>{t('menu.nodes')}</h1>
        <Button icon={<ReloadOutlined />} onClick={() => nodes.refetch()} loading={nodes.isFetching}>
          {t('common.refresh')}
        </Button>
      </div>
      {nodes.isError && <ErrorAlert error={nodes.error} onRetry={() => nodes.refetch()} />}
      <Card>
        <Table<ClusterNode>
          rowKey="code"
          size="middle"
          columns={columns}
          dataSource={nodes.data?.nodes ?? []}
          loading={nodes.isPending}
          pagination={false}
          scroll={{ x: 640 }}
          locale={{ emptyText: t('nodes.empty') }}
        />
      </Card>
    </>
  );
}
