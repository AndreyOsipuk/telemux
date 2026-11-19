// Карточка пользователя (как клиент в 3x-ui): статус, срок с быстрым продлением или
// произвольной датой, включение, лимит подключений и его подключения по машинам сейчас.
// Срок меняется ручкой /api/users/{name}/renew (null - бессрочно); только на мастере.

import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { App, Button, DatePicker, Descriptions, Drawer, Space, Switch, Tag, Tooltip, Typography } from 'antd';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import dayjs from 'dayjs';
import type { Dayjs } from 'dayjs';

import { api } from '@/api/client';
import { describeError } from '@/api/errors';
import { qk, useIsMaster } from '@/api/queries';
import type { User, UserConnections } from '@/api/types';
import ConnectionsTable from '@/components/ConnectionsTable';
import { formatDate, formatDateTime } from '@/utils/format';

// Новый срок при продлении: от текущего срока, а если он прошёл - от сейчас.
export function renewedExpiration(current: string | null, days: number, now = new Date()): string {
  const base = current && dayjs(current).isAfter(now) ? dayjs(current) : dayjs(now);
  return base.add(days, 'day').toISOString();
}

export type UserStatus = 'active' | 'expired' | 'disabled';

export function userStatus(u: Pick<User, 'enabled' | 'expiration_at'>, now = new Date()): UserStatus {
  if (!u.enabled) return 'disabled';
  if (u.expiration_at && !dayjs(u.expiration_at).isAfter(now)) return 'expired';
  return 'active';
}

const STATUS_COLOR: Record<UserStatus, string> = { active: 'success', expired: 'error', disabled: 'default' };

export function UserStatusTag({ u }: { u: Pick<User, 'enabled' | 'expiration_at'> }) {
  const { t } = useTranslation();
  const s = userStatus(u);
  return <Tag color={STATUS_COLOR[s]}>{t(`users.status.${s}`)}</Tag>;
}

const PRESETS = [30, 90, 365];

export default function UserDrawer({ user, conns, onClose }: { user: User | null; conns?: UserConnections; onClose: () => void }) {
  const { t } = useTranslation();
  const { message } = App.useApp();
  const qc = useQueryClient();
  const isMaster = useIsMaster();
  const [date, setDate] = useState<Dayjs | null>(null);
  const done = (text: string) => {
    message.success(text);
    void qc.invalidateQueries({ queryKey: qk.usersAll });
  };
  const onError = (e: unknown) => message.error(describeError(e, t));
  const renew = useMutation({
    mutationFn: ({ name, at }: { name: string; at: string | null }) => api.renewUser(name, at),
    onSuccess: (_d, v) => {
      setDate(null);
      done(v.at ? t('users.expirationSet', { date: formatDate(v.at) }) : t('users.expirationCleared'));
    },
    onError,
  });
  const toggle = useMutation({
    mutationFn: ({ name, on }: { name: string; on: boolean }) => api.setUserEnabled(name, on),
    onSuccess: () => done(t('common.saved')),
    onError,
  });
  const u = user;
  const writeHint = isMaster ? undefined : t('role.writeOnMaster');

  return (
    <Drawer
      open={u !== null}
      onClose={onClose}
      size="large"
      destroyOnHidden
      title={
        u ? (
          <Space size={8}>
            <span className="mono">{u.username}</span>
            <UserStatusTag u={u} />
          </Space>
        ) : undefined
      }
      data-testid="user-drawer"
    >
      {u && (
        <Space orientation="vertical" size={20} style={{ width: '100%' }}>
          <Descriptions
            size="small"
            bordered
            column={1}
            items={[
              {
                key: 'enabled',
                label: t('users.enabled'),
                children: (
                  <Tooltip title={writeHint}>
                    <Switch
                      size="small"
                      checked={u.enabled}
                      disabled={!isMaster}
                      loading={toggle.isPending}
                      aria-label={t('users.enabled')}
                      onChange={(on) => toggle.mutate({ name: u.username, on })}
                    />
                  </Tooltip>
                ),
              },
              {
                key: 'exp',
                label: t('users.expiration'),
                children: u.expiration_at ? (
                  <Space size={6} wrap>
                    <span>{formatDate(u.expiration_at)}</span>
                    <Typography.Text type={userStatus(u) === 'expired' ? 'danger' : 'secondary'}>
                      {dayjs(u.expiration_at).isAfter(dayjs())
                        ? t('users.daysLeft', { count: Math.ceil(dayjs(u.expiration_at).diff(dayjs(), 'day', true)) })
                        : t('users.expiredAgo', { date: formatDate(u.expiration_at) })}
                    </Typography.Text>
                  </Space>
                ) : (
                  <Tag>{t('users.noExpiration')}</Tag>
                ),
              },
              { key: 'max', label: t('users.maxConns'), children: u.max_tcp_conns ?? t('users.maxDefault') },
              { key: 'created', label: t('users.createdAt'), children: formatDateTime(u.created_at) },
              {
                key: 'now',
                label: t('users.active'),
                children: conns ? t('users.nowLine', { conns: conns.connections, machines: conns.machines }) : '-',
              },
            ]}
          />
          <div>
            <Typography.Title level={5}>{t('users.changeExpiration')}</Typography.Title>
            <Space wrap>
              {PRESETS.map((d) => (
                <Tooltip key={d} title={writeHint ?? t('users.extendHint')}>
                  <Button
                    disabled={!isMaster}
                    loading={renew.isPending && renew.variables?.at === renewedExpiration(u.expiration_at, d)}
                    onClick={() => renew.mutate({ name: u.username, at: renewedExpiration(u.expiration_at, d) })}
                  >
                    {t('users.extend', { count: d })}
                  </Button>
                </Tooltip>
              ))}
              <Button disabled={!isMaster || !u.expiration_at} onClick={() => renew.mutate({ name: u.username, at: null })}>
                {t('users.makeUnlimited')}
              </Button>
            </Space>
            <Space wrap style={{ marginTop: 8 }}>
              <DatePicker
                value={date}
                onChange={setDate}
                disabled={!isMaster}
                format="DD.MM.YYYY"
                placeholder={t('users.pickDate')}
                aria-label={t('users.pickDate')}
              />
              <Button
                type="primary"
                disabled={!isMaster || !date}
                onClick={() => date && renew.mutate({ name: u.username, at: date.endOf('day').toISOString() })}
              >
                {t('users.setDate')}
              </Button>
            </Space>
          </div>
          <div>
            <Typography.Title level={5}>{t('users.connectionsNow')}</Typography.Title>
            <ConnectionsTable fixedUser={u.username} defaultMinConnections={0} />
          </div>
        </Space>
      )}
    </Drawer>
  );
}
