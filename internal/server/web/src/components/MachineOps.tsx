// Вкладки карточки машины по ADR 0007: «Нагрузка» - графики истории (CPU, память,
// диск, сеть, подключения) за выбранное окно; «Связь» - проверка с мастера по кнопке:
// свежесть агента и каждая точка входа серверов машины (TCP, у web ещё TLS).

import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Alert, Button, Card, Col, Descriptions, Empty, Row, Segmented, Space, Table, Tag, Tooltip, Typography, theme } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { ApiOutlined } from '@ant-design/icons';

import { METRIC_RANGES, useCheckMachine, useIsMaster, useMachineMetrics } from '@/api/queries';
import type { MetricRange } from '@/api/queries';
import type { EndpointCheck, MetricPoint } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import LineChart, { ChartLegend } from '@/components/LineChart';
import type { ChartSeries } from '@/components/LineChart';
import { useDuration } from '@/hooks/useRelativeTime';
import { formatDateTime } from '@/utils/format';

// Бит/с -> короткая строка: 950 бит/с, 12.3 Кбит/с, 4.1 Мбит/с, 1.2 Гбит/с.
export function formatBps(v: number, units: [string, string, string, string]): string {
  const steps = [1, 1e3, 1e6, 1e9];
  let i = 0;
  while (i < 3 && Math.abs(v) >= steps[i + 1]) i++;
  const n = v / steps[i];
  return `${n >= 100 || i === 0 ? Math.round(n) : Number(n.toFixed(1))} ${units[i]}`;
}

const pct = (v: number) => `${Math.round(v)}%`;
const count = (v: number) => String(Math.round(v));

// Ряды графиков из точек ручки (поле -> ряд).
function pick(data: MetricPoint[], f: (p: MetricPoint) => number | null): (number | null)[] {
  return data.map(f);
}

export function MetricsTab({ code }: { code: string }) {
  const { t } = useTranslation();
  const { token } = theme.useToken();
  const [range, setRange] = useState<MetricRange>('24h');
  const q = useMachineMetrics(code, range);
  const data = q.data?.data ?? [];
  const units: [string, string, string, string] = [t('metrics.bps'), t('metrics.kbps'), t('metrics.mbps'), t('metrics.gbps')];

  const charts = useMemo(() => {
    const c = { cpu: token.colorPrimary, mem: token.purple, disk: token.orange, rx: token.colorSuccess, tx: token.colorPrimary, conns: token.colorPrimary, users: token.colorSuccess };
    const host: ChartSeries[] = [
      { key: 'cpu', label: t('runtime.cpu'), color: c.cpu, values: pick(data, (p) => p.cpu) },
      { key: 'mem', label: t('runtime.mem'), color: c.mem, values: pick(data, (p) => p.mem) },
      { key: 'disk', label: t('runtime.disk'), color: c.disk, values: pick(data, (p) => p.disk) },
    ];
    const net: ChartSeries[] = [
      { key: 'rx', label: t('metrics.rx'), color: c.rx, values: pick(data, (p) => p.rx_bps) },
      { key: 'tx', label: t('metrics.tx'), color: c.tx, values: pick(data, (p) => p.tx_bps) },
    ];
    const conns: ChartSeries[] = [
      { key: 'conns', label: t('metrics.connections'), color: c.conns, values: pick(data, (p) => p.connections) },
      { key: 'users', label: t('metrics.usersOnline'), color: c.users, values: pick(data, (p) => p.users_online) },
    ];
    return { host, net, conns };
  }, [data, t, token]);

  const times = useMemo(() => data.map((p) => Date.parse(p.at)), [data]);
  const longRange = range === '7d';

  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }} data-testid="machine-metrics">
      <Space wrap style={{ justifyContent: 'space-between', width: '100%' }}>
        <Segmented<MetricRange>
          value={range}
          onChange={setRange}
          options={(Object.keys(METRIC_RANGES) as MetricRange[]).map((r) => ({ value: r, label: t(`metrics.range.${r}`) }))}
          aria-label={t('metrics.rangeAria')}
        />
        <Typography.Text type="secondary">{t('metrics.step', { step: METRIC_RANGES[range].step })}</Typography.Text>
      </Space>
      {q.isError && <ErrorAlert error={q.error} onRetry={() => q.refetch()} />}
      {q.isSuccess && data.length === 0 ? (
        <Empty description={t('metrics.empty')} />
      ) : (
        <Row gutter={[16, 16]}>
          <Col span={24}>
            <Card size="small" title={t('metrics.host')} loading={q.isPending}>
              <LineChart times={times} series={charts.host} format={pct} yMax={100} longRange={longRange} testId="chart-host" />
              <ChartLegend series={charts.host} />
            </Card>
          </Col>
          <Col span={24}>
            <Card size="small" title={t('metrics.net')} loading={q.isPending}>
              <LineChart times={times} series={charts.net} format={(v) => formatBps(v, units)} longRange={longRange} testId="chart-net" />
              <ChartLegend series={charts.net} />
            </Card>
          </Col>
          <Col span={24}>
            <Card size="small" title={t('metrics.load')} loading={q.isPending}>
              <LineChart times={times} series={charts.conns} format={count} longRange={longRange} testId="chart-conns" />
              <ChartLegend series={charts.conns} />
            </Card>
          </Col>
        </Row>
      )}
    </Space>
  );
}

function Probe({ ok, ms, error }: { ok: boolean; ms: number; error?: string }) {
  const { t } = useTranslation();
  return (
    <Tooltip title={error}>
      <Tag color={ok ? 'success' : 'error'}>{ok ? t('check.okMs', { ms }) : t('check.fail')}</Tag>
    </Tooltip>
  );
}

export function CheckTab({ code }: { code: string }) {
  const { t } = useTranslation();
  const duration = useDuration();
  const isMaster = useIsMaster();
  const check = useCheckMachine();
  const r = check.data;

  const columns: ColumnsType<EndpointCheck> = [
    {
      title: t('check.server'),
      key: 'server',
      render: (_v, e) => (
        <span>
          <span className="mono">{e.server_no}</span> {e.server_name}
        </span>
      ),
    },
    { title: t('check.protocol'), dataIndex: 'protocol', width: 70, render: (v: string) => <Tag>{v}</Tag> },
    {
      title: t('check.address'),
      key: 'addr',
      render: (_v, e) => (
        <Space size={4}>
          <span className="mono">
            {e.host}:{e.port}
          </span>
          {e.via_relay && <Tag>{t('check.viaRelay')}</Tag>}
        </Space>
      ),
    },
    { title: 'TCP', key: 'tcp', width: 110, render: (_v, e) => <Probe {...e.tcp} /> },
    {
      title: 'TLS',
      key: 'tls',
      width: 170,
      render: (_v, e) =>
        e.tls ? (
          <Space size={4}>
            <Probe ok={e.tls.ok} ms={e.tls.ms} error={e.tls.error} />
            {e.tls.days_left !== null && e.tls.days_left !== undefined && (
              <Tooltip title={e.tls.not_after ? formatDateTime(e.tls.not_after) : undefined}>
                <Tag color={e.tls.days_left < 14 ? 'warning' : 'default'}>{t('check.daysLeft', { count: e.tls.days_left })}</Tag>
              </Tooltip>
            )}
          </Space>
        ) : (
          <Typography.Text type="secondary">-</Typography.Text>
        ),
    },
  ];

  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }} data-testid="machine-check">
      <Space wrap>
        <Tooltip title={isMaster ? undefined : t('check.onMaster')}>
          <Button type="primary" icon={<ApiOutlined />} loading={check.isPending} disabled={!isMaster} onClick={() => check.mutate(code)}>
            {t('check.run')}
          </Button>
        </Tooltip>
        <Typography.Text type="secondary">{t('check.hint')}</Typography.Text>
      </Space>
      {check.isError && <ErrorAlert error={check.error} />}
      {r && (
        <>
          <Alert
            type={r.ok ? 'success' : 'error'}
            showIcon
            title={r.ok ? t('check.allOk') : t('check.someFailed')}
            description={t('check.checkedAt', { at: formatDateTime(r.checked_at) })}
          />
          <Descriptions
            size="small"
            bordered
            column={1}
            title={t('check.agent')}
            items={[
              {
                key: 'state',
                label: t('check.agentState'),
                children: r.agent.silent ? (
                  <Tag color="error">{r.agent.seen_at ? t('check.silent') : t('fleet.noAgent')}</Tag>
                ) : r.agent.fresh ? (
                  <Tag color="success">{t('check.fresh')}</Tag>
                ) : (
                  <Tag color="warning">{t('status.stale')}</Tag>
                ),
              },
              {
                key: 'seen',
                label: t('drawer.seenAt'),
                children:
                  r.agent.seen_at && r.agent.age_seconds !== null && r.agent.age_seconds !== undefined ? (
                    <Tooltip title={formatDateTime(r.agent.seen_at)}>{t('check.ago', { d: duration(r.agent.age_seconds) })}</Tooltip>
                  ) : (
                    '-'
                  ),
              },
            ]}
          />
          <Table<EndpointCheck>
            rowKey={(e) => `${e.endpoint_id}-${e.via_relay}`}
            size="small"
            columns={columns}
            dataSource={r.endpoints}
            pagination={false}
            scroll={{ x: 640 }}
            locale={{ emptyText: t('check.noEndpoints') }}
            title={() => <Typography.Text strong>{t('check.endpoints')}</Typography.Text>}
          />
          {r.truncated && <Alert type="warning" showIcon title={t('check.truncated')} />}
        </>
      )}
    </Space>
  );
}
