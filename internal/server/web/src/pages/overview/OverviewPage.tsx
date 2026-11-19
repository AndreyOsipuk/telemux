// «Обзор» - главная страница панели, по образцу дашборда 3x-ui. Сверху плитки:
// машины (всего, агент в сети, молчат), подключения (всего, пользователей онлайн),
// пары (в порядке, с проблемами), сертификаты (истекают меньше чем через 14 дней,
// не выпущены). Ниже - машины компактной таблицей с нагрузкой, подключениями и
// версиями; клик по строке открывает карточку машины. Всё - одним запросом
// /api/v1/overview, обновление раз в 30 секунд.

import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router';
import { Alert, Button, Card, Col, Row, Space, Statistic, Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { ReloadOutlined } from '@ant-design/icons';

import { useOverview } from '@/api/queries';
import type { Machine, Overview } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import { KindTag } from '@/components/FleetTags';
import MachineDrawer from '@/components/MachineDrawer';
import { useRuntimeColumns } from '@/components/MachineCells';
import './OverviewPage.css';

function Tile({
  title,
  testId,
  onClick,
  children,
}: {
  title: string;
  testId: string;
  onClick?: () => void;
  children: React.ReactNode;
}) {
  return (
    <Card
      size="small"
      title={title}
      data-testid={testId}
      hoverable={!!onClick}
      onClick={onClick}
      className="overview-tile"
    >
      <Space size={24} wrap>
        {children}
      </Space>
    </Card>
  );
}

function Tiles({ o }: { o: Overview }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  return (
    <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
      <Col xs={24} sm={12} xl={6}>
        <Tile title={t('overview.machines')} testId="tile-machines" onClick={() => navigate('/nodes')}>
          <Statistic title={t('overview.total')} value={o.machines.total} />
          <Statistic title={t('overview.agentsOnline')} value={o.machines.online} styles={{ content: { color: 'var(--ant-color-success)' } }} />
          <Statistic
            title={t('overview.agentsSilent')}
            value={o.machines.silent}
            styles={{ content: o.machines.silent > 0 ? { color: 'var(--ant-color-error)' } : undefined }}
          />
        </Tile>
      </Col>
      <Col xs={24} sm={12} xl={6}>
        <Tile title={t('overview.connections')} testId="tile-connections" onClick={() => navigate('/connections')}>
          <Statistic title={t('overview.total')} value={o.connections.total} />
          <Statistic title={t('overview.usersOnline')} value={o.connections.users_online} />
        </Tile>
      </Col>
      <Col xs={24} sm={12} xl={6}>
        <Tile title={t('overview.pairs')} testId="tile-pairs" onClick={() => navigate('/pairs')}>
          <Statistic title={t('overview.pairsOk')} value={o.pairs.ok} styles={{ content: { color: 'var(--ant-color-success)' } }} />
          <Statistic
            title={t('overview.pairsProblem')}
            value={o.pairs.problem}
            styles={{ content: o.pairs.problem > 0 ? { color: 'var(--ant-color-error)' } : undefined }}
          />
        </Tile>
      </Col>
      <Col xs={24} sm={12} xl={6}>
        <Tile title={t('overview.certs')} testId="tile-certs" onClick={() => navigate('/certs')}>
          {o.certs ? (
            <>
              <Statistic
                title={t('overview.certsExpiring', { days: o.cert_warn_days })}
                value={o.certs.expiring}
                styles={{ content: o.certs.expiring > 0 ? { color: 'var(--ant-color-error)' } : undefined }}
              />
              <Statistic
                title={t('overview.certsMissing')}
                value={o.certs.missing}
                styles={{ content: o.certs.missing > 0 ? { color: 'var(--ant-color-warning)' } : undefined }}
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

const RUNTIME_KEYS = ['status', 'cpu', 'mem', 'disk', 'load', 'uptime', 'connections', 'online', 'telemt', 'mtg', 'agent'] as const;

export default function OverviewPage() {
  const { t } = useTranslation();
  const overview = useOverview();
  const [opened, setOpened] = useState<Machine | null>(null);
  const runtimeCols = useRuntimeColumns([...RUNTIME_KEYS]);

  const columns: ColumnsType<Machine> = [
    {
      title: t('nodes.code'),
      dataIndex: 'code',
      width: 160,
      fixed: 'left',
      render: (v: string, m) => (
        <Space size={6}>
          <Typography.Text strong className="mono">
            {v}
          </Typography.Text>
          {!m.enabled && <Tag>{t('nodes.disabled')}</Tag>}
        </Space>
      ),
    },
    { title: t('addMachine.kind'), dataIndex: 'kind', width: 90, render: (v: Machine['kind']) => <KindTag kind={v} /> },
    ...runtimeCols,
  ];

  const o = overview.data;
  return (
    <>
      <div className="page-header">
        <h1>{t('menu.overview')}</h1>
        <Button icon={<ReloadOutlined />} onClick={() => overview.refetch()} loading={overview.isFetching}>
          {t('common.refresh')}
        </Button>
      </div>
      {overview.isError && <ErrorAlert error={overview.error} onRetry={() => overview.refetch()} />}
      {o && <Tiles o={o} />}
      <Card title={t('overview.tableTitle')} extra={<Typography.Text type="secondary">{t('overview.tableHint')}</Typography.Text>} data-testid="overview-machines">
        {o?.truncated && (
          <Alert type="info" showIcon style={{ marginBottom: 12 }} title={t('overview.truncated', { n: o.data.length, total: o.machines.total })} />
        )}
        <Table<Machine>
          rowKey="code"
          size="small"
          columns={columns}
          dataSource={o?.data ?? []}
          loading={overview.isPending}
          pagination={false}
          scroll={{ x: 1350 }}
          locale={{ emptyText: overview.isSuccess ? t('nodes.fleetEmpty') : <span /> }}
          onRow={(m) => ({ onClick: () => setOpened(m), style: { cursor: 'pointer' } })}
        />
      </Card>
      <MachineDrawer machine={opened && (o?.data.find((m) => m.code === opened.code) ?? opened)} onClose={() => setOpened(null)} />
    </>
  );
}
