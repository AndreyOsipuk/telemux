// Пользователи: серверная пагинация {data, paging}, создание, вкл/выкл, продление,
// удаление. Запись - только на мастере (кнопки выключены на реплике, 409 - понятным текстом).
// У каждого пользователя - онлайн ли он и сколько у него подключений по всем машинам:
// один запрос /api/v1/connections/by-user на всю страницу, обновление раз в 15 секунд.

import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { App, Badge, Button, Card, Form, Input, Popconfirm, Space, Switch, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { DeleteOutlined, FieldTimeOutlined, PlusOutlined, ReloadOutlined } from '@ant-design/icons';
import dayjs from 'dayjs';

import { api } from '@/api/client';
import { describeError } from '@/api/errors';
import { qk, useConnectionsByUser, useIsMaster, useUsers } from '@/api/queries';
import type { User, UserConnections } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import { formatDate } from '@/utils/format';

export const RENEW_DAYS = 30;

// Новый срок при продлении: от текущего срока, а если он прошёл - от сейчас.
export function renewedExpiration(current: string | null, days: number, now = new Date()): string {
  const base = current && dayjs(current).isAfter(now) ? dayjs(current) : dayjs(now);
  return base.add(days, 'day').toISOString();
}

export default function UsersPage() {
  const { t } = useTranslation();
  const { message } = App.useApp();
  const qc = useQueryClient();
  const isMaster = useIsMaster();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [newName, setNewName] = useState('');

  const users = useUsers(pageSize, (page - 1) * pageSize);
  const usernames = useMemo(() => (users.data?.data ?? []).map((u) => u.username), [users.data]);
  const conns = useConnectionsByUser(usernames);
  const connsByUser = useMemo(() => {
    const out = new Map<string, UserConnections>();
    for (const c of conns.data?.data ?? []) out.set(c.key, c);
    return out;
  }, [conns.data]);

  // Удалили последнего на последней странице - переходим на новую последнюю,
  // иначе запрос с offset за пределами total покажет пустую таблицу.
  const total = users.data?.paging.total;
  useEffect(() => {
    if (total === undefined) return;
    const last = Math.max(1, Math.ceil(total / pageSize));
    if (page > last) setPage(last);
  }, [total, page, pageSize]);

  const onError = (e: unknown) => message.error(describeError(e, t));
  const onDone = (text: string) => () => {
    message.success(text);
    void qc.invalidateQueries({ queryKey: qk.usersAll });
  };

  const create = useMutation({
    mutationFn: (username: string) => api.createUser({ username }),
    onSuccess: () => {
      setNewName('');
      onDone(t('users.created'))();
    },
    onError,
  });
  const remove = useMutation({ mutationFn: api.deleteUser, onSuccess: onDone(t('users.deleted')), onError });
  const toggle = useMutation({
    mutationFn: ({ username, on }: { username: string; on: boolean }) => api.setUserEnabled(username, on),
    onSuccess: onDone(t('common.saved')),
    onError,
  });
  const renew = useMutation({
    mutationFn: (u: User) => api.renewUser(u.username, renewedExpiration(u.expiration_at, RENEW_DAYS)),
    onSuccess: onDone(t('users.renewed', { days: RENEW_DAYS })),
    onError,
  });

  const writeHint = isMaster ? undefined : t('role.writeOnMaster');

  const columns: ColumnsType<User> = [
    {
      title: t('users.username'),
      dataIndex: 'username',
      render: (v: string) => <Typography.Text strong>{v}</Typography.Text>,
    },
    {
      title: t('users.online'),
      key: 'online',
      align: 'center',
      width: 110,
      render: (_v, u) => {
        const c = connsByUser.get(u.username);
        if (!c) return <Typography.Text type="secondary">-</Typography.Text>;
        return c.online ? (
          <Badge status="success" text={t('status.online')} />
        ) : (
          <Typography.Text type="secondary">{t('users.offline')}</Typography.Text>
        );
      },
    },
    {
      title: (
        <Tooltip title={t('users.activeHint')}>
          <span>{t('users.active')}</span>
        </Tooltip>
      ),
      key: 'active',
      align: 'center',
      width: 130,
      render: (_v, u) => {
        const c = connsByUser.get(u.username);
        if (!c) return <Typography.Text type="secondary">-</Typography.Text>;
        return (
          <Tooltip title={c.connections > 0 ? t('users.onMachines', { n: c.machines }) : undefined}>
            <span className="mono" data-testid={`active-${u.username}`}>
              {c.connections}
            </span>
          </Tooltip>
        );
      },
    },
    {
      title: t('users.expiration'),
      dataIndex: 'expiration_at',
      render: (v: string | null) => {
        if (!v) return <Tag>{t('users.noExpiration')}</Tag>;
        const expired = dayjs(v).isBefore(dayjs());
        return <Tag color={expired ? 'error' : 'default'}>{formatDate(v)}</Tag>;
      },
    },
    {
      title: t('users.maxConns'),
      dataIndex: 'max_tcp_conns',
      align: 'center',
      render: (v: number | null) => v ?? '-',
    },
    {
      title: t('users.enabled'),
      dataIndex: 'enabled',
      align: 'center',
      render: (v: boolean, u) => (
        <Tooltip title={writeHint}>
          <Switch
            size="small"
            checked={v}
            disabled={!isMaster}
            aria-label={t('users.enabled')}
            onChange={(on) => toggle.mutate({ username: u.username, on })}
          />
        </Tooltip>
      ),
    },
    {
      title: t('common.actions'),
      align: 'right',
      render: (_v, u) => (
        <Space>
          <Tooltip title={writeHint ?? t('users.renew', { days: RENEW_DAYS })}>
            <Button
              size="small"
              icon={<FieldTimeOutlined />}
              disabled={!isMaster}
              onClick={() => renew.mutate(u)}
            >
              +{RENEW_DAYS}
            </Button>
          </Tooltip>
          <Popconfirm
            title={t('users.deleteConfirm', { name: u.username })}
            okText={t('common.delete')}
            cancelText={t('common.cancel')}
            okButtonProps={{ danger: true }}
            onConfirm={() => remove.mutate(u.username)}
            disabled={!isMaster}
          >
            <Tooltip title={writeHint ?? t('common.delete')}>
              <Button size="small" danger icon={<DeleteOutlined />} disabled={!isMaster} aria-label={t('common.delete')} />
            </Tooltip>
          </Popconfirm>
        </Space>
      ),
    },
  ];

  return (
    <>
      <div className="page-header">
        <h1>{t('menu.users')}</h1>
        <Button icon={<ReloadOutlined />} onClick={() => users.refetch()} loading={users.isFetching}>
          {t('common.refresh')}
        </Button>
      </div>
      {users.isError && <ErrorAlert error={users.error} onRetry={() => users.refetch()} />}
      {conns.isError && <ErrorAlert title={t('users.connsFailed')} error={conns.error} onRetry={() => conns.refetch()} />}
      <Card>
        <Form
          layout="inline"
          style={{ marginBottom: 16, rowGap: 8 }}
          onFinish={() => newName.trim() && create.mutate(newName.trim())}
        >
          <Form.Item style={{ flex: 1, minWidth: 200 }}>
            <Input
              value={newName}
              onChange={(e) => setNewName(e.target.value)}
              placeholder={t('users.newPlaceholder')}
              aria-label={t('users.username')}
              disabled={!isMaster}
            />
          </Form.Item>
          <Form.Item style={{ marginInlineEnd: 0 }}>
            <Tooltip title={writeHint}>
              <Button type="primary" htmlType="submit" icon={<PlusOutlined />} disabled={!isMaster || !newName.trim()} loading={create.isPending}>
                {t('users.create')}
              </Button>
            </Tooltip>
          </Form.Item>
        </Form>
        <Table<User>
          rowKey="username"
          size="middle"
          columns={columns}
          dataSource={users.data?.data ?? []}
          loading={users.isPending}
          scroll={{ x: 900 }}
          pagination={{
            current: page,
            pageSize,
            total: users.data?.paging.total ?? 0,
            showSizeChanger: true,
            pageSizeOptions: [20, 50, 100],
            showTotal: (total) => t('common.total', { count: total }),
            onChange: (p, size) => {
              setPage(size !== pageSize ? 1 : p);
              setPageSize(size);
            },
          }}
        />
      </Card>
    </>
  );
}
