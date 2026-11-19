// «Машины»: два раздела.
// 1. Машины флота (/api/v1/machines): код, вид, релей, агент и мост, «видели N назад»,
//    локальные проверки и ресурсы из последнего отчёта агента. Постранично. Кнопка
//    «Добавить машину» и у каждой машины - «Команда установки» агента.
// 2. Узлы управления кластером (/api/nodes) - мастер и реплики самого telemux: роль,
//    адрес (скрыт по умолчанию, как в 3x-ui), статус по последнему heartbeat.

import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useIsFetching, useQueryClient } from '@tanstack/react-query';
import { Button, Card, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { EyeInvisibleOutlined, EyeOutlined, PlusOutlined, ReloadOutlined } from '@ant-design/icons';

import { useIsMaster, useMachines, useNodes } from '@/api/queries';
import type { ClusterNode, Machine, MachineStatus } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import { CheckTags, KindTag, ResourceTags, resourceRows } from '@/components/FleetTags';
import { JoinCommandButton } from '@/components/JoinCommand';
import { usePaging } from '@/hooks/usePaging';
import StatusCell from '@/components/StatusDot';
import { secondsSince, useNow, useRelativeTime } from '@/hooks/useRelativeTime';
import { formatDateTime } from '@/utils/format';
import AddMachineModal from './AddMachineModal';
import './NodesPage.css';

// Heartbeat агента - раз в несколько секунд; дольше этого молчания - машина не на связи.
export const NODE_STALE_SECONDS = 90;

export function nodeStatus(n: ClusterNode, now: number): MachineStatus {
  const age = secondsSince(n.last_seen_at, now);
  if (age === null) return 'unknown';
  return age <= NODE_STALE_SECONDS ? 'online' : 'offline';
}

function FleetMachines() {
  const { t } = useTranslation();
  const now = useNow();
  const relative = useRelativeTime();
  const isMaster = useIsMaster();
  const [adding, setAdding] = useState(false);
  const paging = usePaging(20);
  const machines = useMachines(paging.pageSize, paging.offset);
  const { syncTotal } = paging;
  const total = machines.data?.paging.total;
  useEffect(() => syncTotal(total), [syncTotal, total]);

  const columns = useMemo<ColumnsType<Machine>>(
    () => [
      {
        title: t('nodes.code'),
        dataIndex: 'code',
        width: 170,
        render: (v: string, m) => (
          <Space size={8}>
            <KindTag kind={m.kind} />
            <Typography.Text strong className="mono">
              {v}
            </Typography.Text>
            {!m.enabled && <Tag>{t('nodes.disabled')}</Tag>}
          </Space>
        ),
      },
      {
        title: t('fleet.relay'),
        dataIndex: 'relay_code',
        width: 110,
        render: (v: string | undefined) =>
          v ? <span className="mono">{v}</span> : <Typography.Text type="secondary">-</Typography.Text>,
      },
      {
        title: t('fleet.agent'),
        width: 150,
        render: (_v, m) =>
          m.agent_version || m.via ? (
            <div className="name-cell">
              <span className="mono">{m.agent_version ?? '-'}</span>
              {m.via && <span className="remark">{t('fleet.via', { code: m.via })}</span>}
            </div>
          ) : (
            <Typography.Text type="secondary">-</Typography.Text>
          ),
      },
      {
        title: t('fleet.lastSeen'),
        dataIndex: 'seen_at',
        width: 140,
        render: (v: string | null) =>
          v ? (
            <Tooltip title={formatDateTime(v)}>{relative(secondsSince(v, now))}</Tooltip>
          ) : (
            <Typography.Text type="secondary">{t('fleet.noAgent')}</Typography.Text>
          ),
      },
      {
        title: t('fleet.checks'),
        width: 200,
        render: (_v, m) => <CheckTags checks={m.checks} />,
      },
      {
        title: t('fleet.resources'),
        render: (_v, m) => <ResourceTags rows={resourceRows(m.resources, m.observed)} />,
      },
      {
        title: t('common.actions'),
        width: 190,
        render: (_v, m) => <JoinCommandButton code={m.code} hasAgent={m.seen_at !== null} />,
      },
    ],
    [t, now, relative],
  );

  return (
    <Card
      title={t('nodes.fleetTitle')}
      extra={
        <Space wrap>
          <Typography.Text type="secondary">{t('nodes.fleetHint')}</Typography.Text>
          <Tooltip title={isMaster ? undefined : t('role.writeOnMaster')}>
            <Button type="primary" icon={<PlusOutlined />} disabled={!isMaster} onClick={() => setAdding(true)}>
              {t('addMachine.open')}
            </Button>
          </Tooltip>
        </Space>
      }
      style={{ marginBottom: 16 }}
      data-testid="fleet-machines"
    >
      <AddMachineModal open={adding} onClose={() => setAdding(false)} />
      {machines.isError && <ErrorAlert error={machines.error} onRetry={() => machines.refetch()} />}
      <Table<Machine>
        rowKey="code"
        size="middle"
        columns={columns}
        dataSource={machines.data?.data ?? []}
        loading={machines.isPending || (machines.isFetching && machines.isPlaceholderData)}
        scroll={{ x: 1190 }}
        locale={{ emptyText: machines.isSuccess ? t('nodes.fleetEmpty') : <span /> }}
        pagination={paging.pagination(total)}
      />
    </Card>
  );
}

export default function NodesPage() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const nodes = useNodes();
  // Кнопка «Обновить» - для обоих разделов.
  const fetching = useIsFetching({ predicate: (q) => q.queryKey[0] === 'machines' || q.queryKey[0] === 'nodes' });
  const refreshAll = () =>
    qc.invalidateQueries({ predicate: (q) => q.queryKey[0] === 'machines' || q.queryKey[0] === 'nodes' });
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
        <Button icon={<ReloadOutlined />} onClick={() => void refreshAll()} loading={fetching > 0}>
          {t('common.refresh')}
        </Button>
      </div>
      <FleetMachines />
      <Card
        title={t('nodes.clusterTitle')}
        extra={<Typography.Text type="secondary">{t('nodes.clusterHint')}</Typography.Text>}
        data-testid="cluster-nodes"
      >
        {nodes.isError && <ErrorAlert error={nodes.error} onRetry={() => nodes.refetch()} />}
        <Table<ClusterNode>
          rowKey="code"
          size="middle"
          columns={columns}
          dataSource={nodes.data?.nodes ?? []}
          loading={nodes.isPending}
          pagination={false}
          scroll={{ x: 640 }}
          locale={{ emptyText: nodes.isSuccess ? t('nodes.clusterEmpty') : <span /> }}
        />
      </Card>
    </>
  );
}
