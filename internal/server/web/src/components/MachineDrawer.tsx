// Карточка машины (Drawer) - всё, что мастер знает о машине, по вкладкам:
// «Состояние» (runtime-снимок целиком), «Ресурсы и ревизии» (применённое и желаемое,
// локальные проверки), «Подключения», «Ошибки журнала» за 24 ч, «История применений»
// и «Сертификаты». Вкладка грузит данные только когда её открыли.

import { useState } from 'react';
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Alert, Descriptions, Drawer, Space, Table, Tabs, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';

import { useMachineRuntime } from '@/api/queries';
import type { Machine, ObservedResource } from '@/api/types';
import CertsTable from '@/components/CertsTable';
import ConnectionsTable from '@/components/ConnectionsTable';
import ErrorAlert from '@/components/ErrorAlert';
import { CheckTags, KindTag, ResourceTags, resourceRows } from '@/components/FleetTags';
import LogErrorsTable from '@/components/LogErrorsTable';
import { PctText, RuntimeStatusCell } from '@/components/MachineCells';
import TargetHistory from '@/pages/pairs/TargetHistory';
import { secondsSince, useDuration, useNow, useRelativeTime } from '@/hooks/useRelativeTime';
import { formatDateTime } from '@/utils/format';

export type MachineTab = 'state' | 'resources' | 'connections' | 'logErrors' | 'history' | 'certs';

const dash = <Typography.Text type="secondary">-</Typography.Text>;
const text = (v: string | number | undefined | null) =>
  v === undefined || v === null || v === '' ? dash : <span className="mono">{v}</span>;

function ApiOk({ ok, error }: { ok: boolean; error?: string }) {
  const { t } = useTranslation();
  return (
    <Space size={6} wrap>
      <Tag color={ok ? 'success' : 'error'}>{ok ? t('drawer.apiOk') : t('drawer.apiDown')}</Tag>
      {!ok && error && <Typography.Text type="danger">{error}</Typography.Text>}
    </Space>
  );
}

// «Состояние»: runtime-снимок машины понятными подписями.
function StateTab({ code }: { code: string }) {
  const { t } = useTranslation();
  const now = useNow();
  const relative = useRelativeTime();
  const duration = useDuration();
  const rt = useMachineRuntime(code);
  if (rt.isError) return <ErrorAlert error={rt.error} onRetry={() => rt.refetch()} />;
  if (!rt.data) return <Typography.Text type="secondary">{t('drawer.loading')}</Typography.Text>;
  const r = rt.data;
  if (!r.seen_at) return <Alert type="info" showIcon title={t('drawer.noRuntime')} />;
  const mbps = (v: number) => t('drawer.mbps', { value: Number(v.toFixed(1)) });
  const item = (key: string, label: string, children: ReactNode) => ({ key, label, children });
  const report = [
    item(
      'seen',
      t('drawer.seenAt'),
      <Space size={6}>
        <Tooltip title={formatDateTime(r.seen_at)}>{relative(secondsSince(r.seen_at, now))}</Tooltip>
        {r.stale && <Tag color="warning">{t('status.stale')}</Tag>}
      </Space>,
    ),
    item('agent', t('drawer.agentVersion'), text(r.agent_version)),
  ];
  const host = r.host
    ? [
        item('cpu', t('runtime.cpu'), <PctText value={r.host.cpu_pct} />),
        item('mem', t('runtime.mem'), <PctText value={r.host.mem_pct} />),
        item('disk', t('runtime.disk'), <PctText value={r.host.disk_pct} />),
        item('load', t('drawer.load1'), text(Number(r.host.load1.toFixed(2)))),
        item('rx', t('drawer.netRx'), mbps(r.host.net_rx_mbps)),
        item('tx', t('drawer.netTx'), mbps(r.host.net_tx_mbps)),
        item('uptime', t('runtime.uptime'), duration(r.host.uptime_s)),
      ]
    : [item('none', t('drawer.host'), t('drawer.noHost'))];
  const telemt = r.telemt
    ? [
        item('version', t('drawer.version'), text(r.telemt.version)),
        item('api', t('drawer.api'), <ApiOk ok={r.telemt.api_ok} error={r.telemt.error} />),
        item('configured', t('drawer.usersConfigured'), text(r.telemt.users_configured)),
        item('effective', t('drawer.usersEffective'), text(r.telemt.users_effective)),
        item('conns', t('drawer.connectionsTotal'), text(r.telemt.connections_total)),
        item('tls', t('drawer.tlsDomain'), text(r.telemt.tls_domain)),
        item('median', t('drawer.maxConnsMedian'), text(r.telemt.max_tcp_conns_median)),
        item('sha', t('drawer.configSha'), text(r.telemt.config_sha256?.slice(0, 12))),
      ]
    : [item('none', 'telemt', t('drawer.noProduct'))];
  const mtg = r.mtg
    ? [
        item('version', t('drawer.version'), text(r.mtg.version)),
        item('api', t('drawer.api'), <ApiOk ok={r.mtg.api_ok} error={r.mtg.error} />),
        item('conns', t('drawer.connectionsTotal'), text(r.mtg.connections_total)),
      ]
    : [item('none', 'mtg', t('drawer.noProduct'))];
  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }} data-testid="machine-state">
      <Descriptions size="small" bordered column={1} title={t('drawer.report')} items={report} />
      <Descriptions size="small" bordered column={1} title={t('drawer.host')} items={host} />
      <Descriptions size="small" bordered column={1} title="telemt" items={telemt} />
      <Descriptions size="small" bordered column={1} title="mtg" items={mtg} />
    </Space>
  );
}

// «Ресурсы и ревизии»: назначенные ресурсы, применённое агентом против желаемого, проверки.
function ResourcesTab({ m }: { m: Machine }) {
  const { t } = useTranslation();
  const desired = m.observed
    .filter((o) => o.desired_revision !== null && o.desired_revision !== undefined)
    .map((o) => ({ resource: o.resource, revision: o.desired_revision as number }));
  const columns: ColumnsType<ObservedResource> = [
    { title: t('history.resource'), dataIndex: 'resource', render: (v: string) => <span className="mono">{v}</span> },
    { title: t('drawer.applied'), dataIndex: 'revision', width: 100, align: 'center', render: (v: number) => <span className="mono">{v}</span> },
    {
      title: t('drawer.desired'),
      dataIndex: 'desired_revision',
      width: 100,
      align: 'center',
      render: (v: number | null | undefined) => (v === null || v === undefined ? dash : <span className="mono">{v}</span>),
    },
    { title: t('history.status'), dataIndex: 'status', width: 120, render: (v: string) => <Tag>{t(`applyStatus.${v}`, { defaultValue: v })}</Tag> },
    { title: t('drawer.reported'), dataIndex: 'seen_at', width: 170, render: (v: string) => formatDateTime(v) },
    { title: t('history.error'), dataIndex: 'error', ellipsis: true, render: (v?: string) => (v ? <Typography.Text type="danger">{v}</Typography.Text> : '-') },
  ];
  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }}>
      <div>
        <Typography.Title level={5}>{t('fleet.resources')}</Typography.Title>
        <ResourceTags rows={resourceRows(m.resources, m.observed, desired)} />
      </div>
      <Table<ObservedResource>
        rowKey="resource"
        size="small"
        columns={columns}
        dataSource={m.observed}
        pagination={false}
        scroll={{ x: 700 }}
        locale={{ emptyText: t('drawer.noObserved') }}
      />
      <div>
        <Typography.Title level={5}>{t('fleet.checks')}</Typography.Title>
        {m.checks.length > 0 ? <CheckTags checks={m.checks} /> : <Typography.Text type="secondary">{t('drawer.noChecks')}</Typography.Text>}
      </div>
    </Space>
  );
}

interface MachineDrawerProps {
  machine: Machine | null;
  onClose: () => void;
  initialTab?: MachineTab;
}

export default function MachineDrawer({ machine, onClose, initialTab = 'state' }: MachineDrawerProps) {
  const { t } = useTranslation();
  const [tab, setTab] = useState<MachineTab>(initialTab);
  const m = machine;
  return (
    <Drawer
      open={m !== null}
      onClose={() => {
        setTab(initialTab);
        onClose();
      }}
      size="large"
      destroyOnHidden
      title={
        m ? (
          <Space size={8} wrap>
            <KindTag kind={m.kind} />
            <span className="mono">{t('drawer.title', { code: m.code })}</span>
            <RuntimeStatusCell m={m} />
            {!m.enabled && <Tag>{t('nodes.disabled')}</Tag>}
          </Space>
        ) : undefined
      }
      data-testid="machine-drawer"
    >
      {m && (
        <Tabs
          activeKey={tab}
          onChange={(k) => setTab(k as MachineTab)}
          items={[
            { key: 'state', label: t('drawer.tabs.state'), children: <StateTab code={m.code} /> },
            { key: 'resources', label: t('drawer.tabs.resources'), children: <ResourcesTab m={m} /> },
            { key: 'connections', label: t('drawer.tabs.connections'), children: <ConnectionsTable fixedMachine={m.code} defaultMinConnections={0} /> },
            { key: 'logErrors', label: t('drawer.tabs.logErrors'), children: <LogErrorsTable machine={m.code} /> },
            { key: 'history', label: t('drawer.tabs.history'), children: <TargetHistory code={m.code} /> },
            { key: 'certs', label: t('drawer.tabs.certs'), children: <CertsTable machine={m.code} /> },
          ]}
        />
      )}
    </Drawer>
  );
}
