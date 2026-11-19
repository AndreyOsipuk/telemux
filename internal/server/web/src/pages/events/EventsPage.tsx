// «События»: лента тревог мастера по машинам (ADR 0007) - машина замолчала (down),
// вернулась (up), агент не смог применить ревизию (apply_failed). Хранение 30 дней;
// панель забирает ленту целиком и показывает новые первыми, с фильтром по машине и виду.

import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Alert, Button, Card, Select, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { ReloadOutlined } from '@ant-design/icons';

import { useEvents } from '@/api/queries';
import type { MachineEvent, MachineEventKind } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import { secondsSince, useNow, useRelativeTime } from '@/hooks/useRelativeTime';
import { formatDateTime } from '@/utils/format';

const KINDS: MachineEventKind[] = ['down', 'up', 'apply_failed'];
const KIND_COLOR: Record<MachineEventKind, string> = { down: 'error', up: 'success', apply_failed: 'warning' };

export function EventKindTag({ kind }: { kind: MachineEventKind }) {
  const { t } = useTranslation();
  return <Tag color={KIND_COLOR[kind] ?? 'default'}>{t(`events.kinds.${kind}`, { defaultValue: kind })}</Tag>;
}

export function filterEvents(events: MachineEvent[], machine?: string, kind?: MachineEventKind): MachineEvent[] {
  return events.filter((e) => (!machine || e.machine === machine) && (!kind || e.kind === kind));
}

export default function EventsPage() {
  const { t } = useTranslation();
  const now = useNow();
  const relative = useRelativeTime();
  const q = useEvents();
  const [machine, setMachine] = useState<string>();
  const [kind, setKind] = useState<MachineEventKind>();
  const [page, setPage] = useState({ current: 1, pageSize: 20 });

  const all = useMemo(() => q.data?.data ?? [], [q.data]);
  const machines = useMemo(() => [...new Set(all.map((e) => e.machine))].sort(), [all]);
  const rows = useMemo(() => filterEvents(all, machine, kind), [all, machine, kind]);

  const columns = useMemo<ColumnsType<MachineEvent>>(
    () => [
      {
        title: t('events.at'),
        dataIndex: 'at',
        width: 190,
        render: (v: string) => (
          <Tooltip title={relative(secondsSince(v, now))}>
            <span className="mono">{formatDateTime(v)}</span>
          </Tooltip>
        ),
      },
      { title: t('events.machine'), dataIndex: 'machine', width: 130, render: (v: string) => <Typography.Text strong className="mono">{v}</Typography.Text> },
      { title: t('events.kind'), dataIndex: 'kind', width: 160, render: (v: MachineEventKind) => <EventKindTag kind={v} /> },
      { title: t('events.detail'), dataIndex: 'detail', render: (v: string) => v || <Typography.Text type="secondary">-</Typography.Text> },
    ],
    [t, now, relative],
  );

  const setFilter = (fn: () => void) => {
    fn();
    setPage((p) => ({ ...p, current: 1 }));
  };

  return (
    <>
      <div className="page-header">
        <h1>{t('menu.events')}</h1>
        <Button icon={<ReloadOutlined />} onClick={() => q.refetch()} loading={q.isFetching}>
          {t('common.refresh')}
        </Button>
      </div>
      <Alert type="info" showIcon style={{ marginBottom: 16 }} title={t('events.hint')} />
      {q.isError && <ErrorAlert error={q.error} onRetry={() => q.refetch()} />}
      {q.data?.truncated && <Alert type="warning" showIcon style={{ marginBottom: 16 }} title={t('events.truncated')} />}
      <Card>
        <Space wrap style={{ marginBottom: 16 }}>
          <Select<string>
            allowClear
            showSearch
            style={{ width: 180 }}
            placeholder={t('events.filterMachine')}
            aria-label={t('events.filterMachine')}
            value={machine}
            onChange={(v) => setFilter(() => setMachine(v))}
            options={machines.map((m) => ({ value: m, label: m }))}
          />
          <Select<MachineEventKind>
            allowClear
            style={{ width: 200 }}
            placeholder={t('events.filterKind')}
            aria-label={t('events.filterKind')}
            value={kind}
            onChange={(v) => setFilter(() => setKind(v))}
            options={KINDS.map((k) => ({ value: k, label: t(`events.kinds.${k}`) }))}
          />
        </Space>
        <Table<MachineEvent>
          rowKey="id"
          size="middle"
          columns={columns}
          dataSource={rows}
          loading={q.isPending}
          scroll={{ x: 760 }}
          locale={{ emptyText: q.isSuccess ? t('events.empty') : <span /> }}
          pagination={{
            ...page,
            total: rows.length,
            showSizeChanger: true,
            showTotal: (n) => t('common.total', { count: n }),
            onChange: (current, pageSize) => setPage({ current, pageSize }),
          }}
        />
      </Card>
    </>
  );
}
