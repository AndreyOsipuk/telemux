// «Серверы» (ADR 0005): что видит клиент - «Сервер N», движок, релей и машина за
// рубежом, точки входа, живые цифры по машинам сервера. Аналог «Входов» 3x-ui.
// Описание пока справочное: бот берёт серверы из своей базы до переключения.

import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Alert, App, Button, Card, Popconfirm, Select, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { DeleteOutlined, EditOutlined, PlusOutlined, ReloadOutlined } from '@ant-design/icons';

import { describeError } from '@/api/errors';
import { useDeleteServer, useIsMaster, useServers } from '@/api/queries';
import type { Server, ServerEndpoint, ServerEngine, ServerFilter, ServerMachineLive, ServerState } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import { StatusDot } from '@/components/StatusDot';
import { usePaging } from '@/hooks/usePaging';
import { secondsSince, useNow, useRelativeTime } from '@/hooks/useRelativeTime';
import ServerForm, { PROTOCOL_COLOR } from './ServerForm';
import { ENGINES, STATES, liveStatus, worstStatus } from './serverModel';

const ENGINE_COLOR: Record<ServerEngine, string> = { telemt: 'blue', mtg: 'purple' };
const STATE_COLOR: Record<ServerState, string> = {
  active: 'success',
  maintenance: 'warning',
  test: 'processing',
  disabled: 'default',
};

export function EngineTag({ engine }: { engine: ServerEngine }) {
  return <Tag color={ENGINE_COLOR[engine]}>{engine}</Tag>;
}

export function StateTag({ state }: { state: ServerState }) {
  const { t } = useTranslation();
  return (
    <Tag color={STATE_COLOR[state]} data-state={state}>
      {t(`servers.states.${state}`)}
    </Tag>
  );
}

function EndpointTag({ ep }: { ep: ServerEndpoint }) {
  const { t } = useTranslation();
  const where = ep.protocol === 'web' ? ep.host : `${ep.host}:${ep.port}`;
  const tip = (
    <div>
      <div className="mono">{where}</div>
      <div>{ep.via_relay ? t('servers.endpoint.viaRelayShort') : t('servers.endpoint.direct')}</div>
      {ep.protocol === 'ee' && ep.tls_domain && <div>{t('servers.endpoint.tlsDomainShort', { domain: ep.tls_domain })}</div>}
      {ep.label && <div>{ep.label}</div>}
      {!ep.enabled && <div>{t('servers.endpoint.disabled')}</div>}
    </div>
  );
  return (
    <Tooltip title={tip}>
      <Tag
        color={ep.enabled ? PROTOCOL_COLOR[ep.protocol] : undefined}
        style={ep.enabled ? undefined : { opacity: 0.5, textDecoration: 'line-through' }}
        data-endpoint={ep.protocol}
        data-enabled={ep.enabled}
      >
        {ep.protocol}
      </Tag>
    </Tooltip>
  );
}

function Machines({ s }: { s: Server }) {
  const { t } = useTranslation();
  if (!s.relay_code && !s.node_code) return <Typography.Text type="secondary">-</Typography.Text>;
  return (
    <Space size={4} wrap>
      <span className="mono">{s.relay_code ?? '-'}</span>
      <span aria-hidden>→</span>
      {s.node_retired ? (
        <Tooltip title={t('servers.nodeRetiredHint')}>
          <Space size={4}>
            {s.node_code && (
              <Typography.Text delete type="secondary" className="mono">
                {s.node_code}
              </Typography.Text>
            )}
            <Tag>{t('servers.retired')}</Tag>
          </Space>
        </Tooltip>
      ) : (
        <span className="mono">{s.node_code ?? '-'}</span>
      )}
      {s.relay_only && (
        <Tooltip title={t('servers.form.relayOnlyHint')}>
          <Tag color="gold">{t('servers.relayOnlyShort')}</Tag>
        </Tooltip>
      )}
    </Space>
  );
}

function LiveCell({ s, now }: { s: Server; now: number }) {
  const { t } = useTranslation();
  const relative = useRelativeTime();
  const machines = [s.live?.relay, s.live?.node].filter((m): m is ServerMachineLive => !!m);
  if (machines.length === 0) return <Typography.Text type="secondary">-</Typography.Text>;
  const statuses = machines.map((m) => liveStatus(m, now));
  const status = worstStatus(statuses) ?? 'unknown';
  const conns = machines.reduce((n, m) => n + m.connections, 0);
  const online = machines.reduce((n, m) => n + m.users_online, 0);
  const tip = (
    <div>
      {machines.map((m, i) => (
        <div key={m.code}>
          <span className="mono">{m.code}</span>: {t('servers.liveLine', { conns: m.connections, online: m.users_online })} (
          {t(`status.${statuses[i]}`)}, {relative(secondsSince(m.seen_at, now))})
        </div>
      ))}
    </div>
  );
  const fresh = status === 'online';
  return (
    <Tooltip title={tip}>
      <Space size={6} data-testid={`live-${s.no}`} data-status={status}>
        <StatusDot status={status} />
        <Typography.Text type={fresh ? undefined : 'secondary'} className="mono">
          {conns} / {online}
        </Typography.Text>
      </Space>
    </Tooltip>
  );
}

type FormState = { mode: 'create' } | { mode: 'edit'; server: Server } | null;

export default function ServersPage() {
  const { t } = useTranslation();
  const { message } = App.useApp();
  const isMaster = useIsMaster();
  const now = useNow();
  const paging = usePaging(20);
  const [filter, setFilter] = useState<ServerFilter>({});
  const [formState, setFormState] = useState<FormState>(null);
  const servers = useServers(filter, paging.pageSize, paging.offset);
  const remove = useDeleteServer();

  const { syncTotal } = paging;
  const total = servers.data?.paging.total;
  useEffect(() => syncTotal(total), [syncTotal, total]);

  const setF = (next: ServerFilter) => {
    setFilter(next);
    paging.reset();
  };

  const writeHint = isMaster ? undefined : t('role.writeOnMaster');

  const columns = useMemo<ColumnsType<Server>>(
    () => [
      { title: t('servers.no'), dataIndex: 'no', width: 60, align: 'right', render: (v: number) => <span className="mono">{v}</span> },
      {
        title: t('servers.name'),
        key: 'name',
        width: 200,
        render: (_v, s) => (
          <Space size={6}>
            {s.emoji && <span aria-hidden>{s.emoji}</span>}
            <Typography.Text strong>{s.name}</Typography.Text>
            {s.country && <Typography.Text type="secondary">{s.country.toUpperCase()}</Typography.Text>}
          </Space>
        ),
      },
      { title: t('servers.engine'), dataIndex: 'engine', width: 90, render: (v: ServerEngine) => <EngineTag engine={v} /> },
      { title: t('servers.machines'), key: 'machines', width: 220, render: (_v, s) => <Machines s={s} /> },
      {
        title: t('servers.endpoints'),
        key: 'endpoints',
        width: 180,
        render: (_v, s) =>
          s.endpoints.length === 0 ? (
            <Typography.Text type="secondary">-</Typography.Text>
          ) : (
            <Space size={2} wrap data-testid={`endpoints-${s.no}`}>
              {[...s.endpoints]
                .sort((a, b) => a.sort_order - b.sort_order)
                .map((ep, i) => (
                  <EndpointTag key={ep.id ?? i} ep={ep} />
                ))}
            </Space>
          ),
      },
      {
        title: (
          <Tooltip title={t('servers.liveHint')}>
            <span>{t('servers.live')}</span>
          </Tooltip>
        ),
        key: 'live',
        width: 130,
        render: (_v, s) => <LiveCell s={s} now={now} />,
      },
      { title: t('servers.state'), dataIndex: 'state', width: 120, render: (v: ServerState) => <StateTag state={v} /> },
      {
        title: t('common.actions'),
        key: 'actions',
        align: 'right',
        width: 110,
        render: (_v, s) => {
          const canDelete = isMaster && s.state === 'disabled';
          return (
            <Space>
              <Tooltip title={t('servers.edit')}>
                <Button
                  size="small"
                  icon={<EditOutlined />}
                  aria-label={t('servers.edit')}
                  onClick={() => setFormState({ mode: 'edit', server: s })}
                />
              </Tooltip>
              <Popconfirm
                title={t('servers.deleteConfirm', { no: s.no, name: s.name })}
                okText={t('common.delete')}
                cancelText={t('common.cancel')}
                okButtonProps={{ danger: true }}
                disabled={!canDelete}
                onConfirm={() =>
                  remove.mutate(s.no, {
                    onSuccess: () => message.success(t('servers.deleted', { no: s.no })),
                    onError: (e) => message.error(describeError(e, t)),
                  })
                }
              >
                <Tooltip title={writeHint ?? (s.state === 'disabled' ? t('common.delete') : t('servers.deleteOnlyDisabled'))}>
                  <Button size="small" danger icon={<DeleteOutlined />} disabled={!canDelete} aria-label={t('common.delete')} />
                </Tooltip>
              </Popconfirm>
            </Space>
          );
        },
      },
    ],
    [t, now, isMaster, writeHint, remove, message],
  );

  return (
    <>
      <div className="page-header">
        <h1>{t('menu.servers')}</h1>
        <Space>
          <Button icon={<ReloadOutlined />} onClick={() => servers.refetch()} loading={servers.isFetching}>
            {t('common.refresh')}
          </Button>
          <Tooltip title={writeHint}>
            <Button type="primary" icon={<PlusOutlined />} disabled={!isMaster} onClick={() => setFormState({ mode: 'create' })}>
              {t('servers.add')}
            </Button>
          </Tooltip>
        </Space>
      </div>
      <Alert type="info" showIcon style={{ marginBottom: 16 }} title={t('servers.referenceOnly')} />
      {servers.isError && <ErrorAlert error={servers.error} onRetry={() => servers.refetch()} />}
      <Card>
        <Space wrap style={{ marginBottom: 16 }}>
          <Select<ServerEngine>
            allowClear
            style={{ width: 160 }}
            placeholder={t('servers.filterEngine')}
            aria-label={t('servers.filterEngine')}
            value={filter.engine}
            onChange={(v) => setF({ ...filter, engine: v })}
            options={ENGINES.map((e) => ({ value: e, label: e }))}
          />
          <Select<ServerState>
            allowClear
            style={{ width: 180 }}
            placeholder={t('servers.filterState')}
            aria-label={t('servers.filterState')}
            value={filter.state}
            onChange={(v) => setF({ ...filter, state: v })}
            options={STATES.map((s) => ({ value: s, label: t(`servers.states.${s}`) }))}
          />
        </Space>
        <Table<Server>
          rowKey="no"
          size="middle"
          columns={columns}
          dataSource={servers.data?.data ?? []}
          loading={servers.isPending || (servers.isFetching && servers.isPlaceholderData)}
          scroll={{ x: 1110 }}
          locale={{ emptyText: servers.isSuccess ? t('servers.empty') : <span /> }}
          pagination={paging.pagination(total)}
        />
      </Card>
      {formState && (
        <ServerForm
          key={formState.mode === 'edit' ? `edit-${formState.server.no}` : 'create'}
          server={formState.mode === 'edit' ? formState.server : null}
          onClose={() => setFormState(null)}
        />
      )}
    </>
  );
}
