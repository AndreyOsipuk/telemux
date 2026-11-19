// «Обзор» - что требует внимания. Плитки итогов (машины, подключения, проблемы,
// сертификаты), список проблем по машинам (агент молчит или не установлен, продукт не
// отвечает, ревизия не применилась или ещё не дошла - бывший «Дрейф»), последние
// тревоги и ошибки журнала за сутки. Сами машины с нагрузкой - на экране «Машины».
// Данные машин - одним запросом /api/v1/overview, обновление раз в 30 секунд.

import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router';
import { Alert, Button, Card, Col, Row, Space, Statistic, Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { ReloadOutlined } from '@ant-design/icons';

import { useEvents, useMachineUsage, useOverview } from '@/api/queries';
import type { Machine, MachineEvent, Overview } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import { KindTag } from '@/components/FleetTags';
import LogErrorsTable from '@/components/LogErrorsTable';
import MachineDrawer from '@/components/MachineDrawer';
import type { MachineTab } from '@/components/MachineDrawer';
import { EventKindTag } from '@/pages/events/EventsPage';
import { formatDateTime } from '@/utils/format';
import './OverviewPage.css';

export type ProblemKind = 'silent' | 'no_agent' | 'degraded' | 'apply_failed' | 'behind';

export interface Problem {
  machine: Machine;
  kind: ProblemKind;
  // Ресурс для apply_failed / behind.
  resource?: string;
  detail?: string;
}

// Проблемы включённых машин; выключенные мастер сознательно не ведёт, выведенные
// (машина за рубежом выведена у всех её серверов) - тоже не проблема.
export function fleetProblems(machines: Machine[], retired: (code: string) => boolean = () => false): Problem[] {
  const out: Problem[] = [];
  for (const m of machines) {
    if (!m.enabled || retired(m.code)) continue;
    if (!m.seen_at) {
      out.push({ machine: m, kind: 'no_agent' });
      continue;
    }
    if (m.runtime_status === 'offline') out.push({ machine: m, kind: 'silent' });
    else if (m.runtime_status === 'degraded') {
      const down = [m.telemt_api_ok === false ? 'telemt' : '', m.mtg_api_ok === false ? 'mtg' : ''].filter(Boolean).join(', ');
      out.push({ machine: m, kind: 'degraded', detail: down || undefined });
    }
    for (const o of m.observed) {
      if (o.status === 'failed') out.push({ machine: m, kind: 'apply_failed', resource: o.resource, detail: o.error });
      else if (o.desired_revision !== null && o.desired_revision !== undefined && o.desired_revision > o.revision)
        out.push({ machine: m, kind: 'behind', resource: o.resource, detail: `${o.revision} → ${o.desired_revision}` });
    }
  }
  return out;
}

const PROBLEM_COLOR: Record<ProblemKind, string> = {
  silent: 'error',
  no_agent: 'default',
  degraded: 'warning',
  apply_failed: 'error',
  behind: 'processing',
};
// Вкладка карточки, где видна причина.
const PROBLEM_TAB: Record<ProblemKind, MachineTab> = {
  silent: 'check',
  no_agent: 'state',
  degraded: 'state',
  apply_failed: 'resources',
  behind: 'resources',
};

function Tile({ title, testId, onClick, children }: { title: string; testId: string; onClick?: () => void; children: React.ReactNode }) {
  return (
    <Card size="small" title={title} data-testid={testId} hoverable={!!onClick} onClick={onClick} className="overview-tile">
      <Space size={24} wrap>
        {children}
      </Space>
    </Card>
  );
}

const red = (n: number) => (n > 0 ? { content: { color: 'var(--ant-color-error)' } } : undefined);

function Tiles({ o, problems }: { o: Overview; problems: number }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  return (
    <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
      <Col xs={24} sm={12} xl={6}>
        <Tile title={t('overview.machines')} testId="tile-machines" onClick={() => navigate('/nodes')}>
          <Statistic title={t('overview.total')} value={o.machines.total} />
          <Statistic title={t('overview.agentsOnline')} value={o.machines.online} styles={{ content: { color: 'var(--ant-color-success)' } }} />
          <Statistic title={t('overview.agentsSilent')} value={o.machines.silent} styles={red(o.machines.silent)} />
        </Tile>
      </Col>
      <Col xs={24} sm={12} xl={6}>
        <Tile title={t('overview.connections')} testId="tile-connections" onClick={() => navigate('/connections')}>
          <Statistic title={t('overview.total')} value={o.connections.total} />
          <Statistic title={t('overview.usersOnline')} value={o.connections.users_online} />
        </Tile>
      </Col>
      <Col xs={24} sm={12} xl={6}>
        <Tile title={t('overview.problems')} testId="tile-problems">
          <Statistic title={t('overview.problemsOpen')} value={problems} styles={red(problems)} />
        </Tile>
      </Col>
      <Col xs={24} sm={12} xl={6}>
        <Tile title={t('overview.certs')} testId="tile-certs">
          {o.certs ? (
            <>
              <Statistic title={t('overview.certsExpiring', { days: o.cert_warn_days })} value={o.certs.expiring} styles={red(o.certs.expiring)} />
              <Statistic
                title={t('overview.certsMissing')}
                value={o.certs.missing}
                styles={o.certs.missing > 0 ? { content: { color: 'var(--ant-color-warning)' } } : undefined}
              />
              <Statistic title={t('overview.total')} value={o.certs.total} />
            </>
          ) : (
            <Typography.Text type="secondary">{t('overview.certsOff')}</Typography.Text>
          )}
        </Tile>
      </Col>
    </Row>
  );
}

const RECENT_EVENTS = 5;

export default function OverviewPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const overview = useOverview();
  const events = useEvents();
  const [opened, setOpened] = useState<{ m: Machine; tab: MachineTab } | null>(null);
  const o = overview.data;
  const usage = useMachineUsage();
  const problems = useMemo(() => fleetProblems(o?.data ?? [], (c) => usage.get(c) === 'retired'), [o, usage]);
  const recent = (events.data?.data ?? []).slice(0, RECENT_EVENTS);

  const columns: ColumnsType<Problem> = [
    {
      title: t('nodes.code'),
      key: 'code',
      width: 150,
      render: (_v, p) => (
        <Space size={6}>
          <KindTag kind={p.machine.kind} />
          <Typography.Text strong className="mono">
            {p.machine.code}
          </Typography.Text>
        </Space>
      ),
    },
    {
      title: t('overview.problem'),
      key: 'kind',
      width: 210,
      render: (_v, p) => <Tag color={PROBLEM_COLOR[p.kind]}>{t(`overview.problemKind.${p.kind}`)}</Tag>,
    },
    {
      title: t('overview.problemDetail'),
      key: 'detail',
      render: (_v, p) => (
        <Space size={6} wrap>
          {p.resource && <span className="mono">{p.resource}</span>}
          {p.detail && <Typography.Text type={p.kind === 'apply_failed' ? 'danger' : 'secondary'}>{p.detail}</Typography.Text>}
        </Space>
      ),
    },
  ];

  return (
    <>
      <div className="page-header">
        <h1>{t('menu.overview')}</h1>
        <Button icon={<ReloadOutlined />} onClick={() => overview.refetch()} loading={overview.isFetching}>
          {t('common.refresh')}
        </Button>
      </div>
      {overview.isError && <ErrorAlert error={overview.error} onRetry={() => overview.refetch()} />}
      {o && <Tiles o={o} problems={problems.length} />}
      {o?.truncated && (
        <Alert type="info" showIcon style={{ marginBottom: 16 }} title={t('overview.truncated', { n: o.data.length, total: o.machines.total })} />
      )}
      <Card title={t('overview.problemsTitle')} extra={<Typography.Text type="secondary">{t('overview.problemsHint')}</Typography.Text>} style={{ marginBottom: 16 }} data-testid="overview-problems">
        <Table<Problem>
          rowKey={(p) => `${p.machine.code}/${p.kind}/${p.resource ?? ''}`}
          size="small"
          columns={columns}
          dataSource={problems}
          loading={overview.isPending}
          pagination={problems.length > 20 ? { pageSize: 20 } : false}
          scroll={{ x: 640 }}
          locale={{ emptyText: overview.isSuccess ? t('overview.noProblems') : <span /> }}
          onRow={(p) => ({ onClick: () => setOpened({ m: p.machine, tab: PROBLEM_TAB[p.kind] }), style: { cursor: 'pointer' } })}
        />
      </Card>
      <Card
        title={t('overview.recentEvents')}
        extra={
          <Button type="link" onClick={() => navigate('/events')}>
            {t('overview.allEvents')}
          </Button>
        }
        style={{ marginBottom: 16 }}
        data-testid="overview-events"
      >
        {events.isError && <ErrorAlert error={events.error} onRetry={() => events.refetch()} />}
        {recent.length === 0 ? (
          <Typography.Text type="secondary">{events.isSuccess ? t('events.empty') : ''}</Typography.Text>
        ) : (
          <Space orientation="vertical" size={6} style={{ width: '100%' }}>
            {recent.map((e: MachineEvent) => (
              <Space key={e.id} size={8} wrap>
                <span className="mono">{formatDateTime(e.at)}</span>
                <Typography.Text strong className="mono">
                  {e.machine}
                </Typography.Text>
                <EventKindTag kind={e.kind} />
                <Typography.Text type="secondary">{e.detail}</Typography.Text>
              </Space>
            ))}
          </Space>
        )}
      </Card>
      <Card title={t('overview.logErrors')} extra={<Typography.Text type="secondary">{t('overview.logErrorsHint')}</Typography.Text>} data-testid="overview-log-errors">
        <LogErrorsTable />
      </Card>
      <MachineDrawer
        key={opened ? `${opened.m.code}-${opened.tab}` : 'none'}
        machine={opened && (o?.data.find((m) => m.code === opened.m.code) ?? opened.m)}
        initialTab={opened?.tab}
        onClose={() => setOpened(null)}
      />
    </>
  );
}
