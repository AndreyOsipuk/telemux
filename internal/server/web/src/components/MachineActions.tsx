// Действия с машиной как с нодой 3x-ui (ADR 0007): включить/выключить и удалить.
// Выключенной машине мастер не публикует новые ревизии и не шлёт по ней тревоги;
// удалить можно только выключенную и ни на что не ссылающуюся (иначе 409 с перечнем
// серверов и машин в тексте - его и показываем).

import { useTranslation } from 'react-i18next';
import { App, Button, Popconfirm, Space, Switch, Tooltip } from 'antd';
import { DeleteOutlined } from '@ant-design/icons';

import { describeError } from '@/api/errors';
import { ApiError } from '@/api/http';
import { useDeleteTarget, useIsMaster, useSetTargetEnabled } from '@/api/queries';
import type { Machine } from '@/api/types';

// Текст ошибки удаления: у machine_in_use бэкенд перечисляет, что ссылается на машину.
export function deleteErrorText(err: unknown, t: (k: string, o?: Record<string, unknown>) => string): string {
  if (err instanceof ApiError && err.status === 409) {
    if (err.code === 'machine_enabled') return t('machine.deleteEnabled');
    if (err.code === 'machine_in_use') return t('machine.inUse', { detail: err.message });
  }
  return describeError(err, t as never);
}

export function MachineEnabledSwitch({ m }: { m: Pick<Machine, 'code' | 'enabled'> }) {
  const { t } = useTranslation();
  const { message } = App.useApp();
  const isMaster = useIsMaster();
  const setEnabled = useSetTargetEnabled();
  const toggle = (enabled: boolean) =>
    setEnabled.mutate(
      { code: m.code, enabled },
      {
        onSuccess: () => message.success(t(enabled ? 'machine.enabledOk' : 'machine.disabledOk', { code: m.code })),
        onError: (e) => message.error(describeError(e, t)),
      },
    );
  const sw = (
    <Switch
      size="small"
      checked={m.enabled}
      loading={setEnabled.isPending}
      disabled={!isMaster}
      aria-label={t('machine.enabledAria', { code: m.code })}
      // Включение - сразу; выключение - через подтверждение (Popconfirm ниже).
      onChange={(on) => on && toggle(true)}
    />
  );
  return (
    // Клик по переключателю не должен открывать карточку машины (клик по строке таблицы).
    <span onClick={(e) => e.stopPropagation()}>
      <Tooltip title={isMaster ? (m.enabled ? t('machine.disableHint') : t('machine.enableHint')) : t('role.writeOnMaster')}>
        {m.enabled ? (
          <Popconfirm
            title={t('machine.disableConfirm', { code: m.code })}
            description={t('machine.disableEffect')}
            okText={t('machine.disable')}
            cancelText={t('common.cancel')}
            okButtonProps={{ danger: true }}
            disabled={!isMaster}
            onConfirm={() => toggle(false)}
          >
            {sw}
          </Popconfirm>
        ) : (
          sw
        )}
      </Tooltip>
    </span>
  );
}

export function MachineDeleteButton({ m, onDeleted }: { m: Pick<Machine, 'code' | 'enabled'>; onDeleted?: () => void }) {
  const { t } = useTranslation();
  const { message, modal } = App.useApp();
  const isMaster = useIsMaster();
  const remove = useDeleteTarget();
  const canDelete = isMaster && !m.enabled;
  const hint = !isMaster ? t('role.writeOnMaster') : m.enabled ? t('machine.deleteOnlyDisabled') : t('common.delete');
  return (
    <span onClick={(e) => e.stopPropagation()}>
      <Popconfirm
        title={t('machine.deleteConfirm', { code: m.code })}
        description={t('machine.deleteEffect')}
        okText={t('common.delete')}
        cancelText={t('common.cancel')}
        okButtonProps={{ danger: true }}
        disabled={!canDelete}
        onConfirm={() =>
          remove.mutate(m.code, {
            onSuccess: () => {
              message.success(t('machine.deleted', { code: m.code }));
              onDeleted?.();
            },
            // Перечень ссылок бывает длинным - в окне, а не во всплывающем сообщении.
            onError: (e) => modal.error({ title: t('machine.deleteFailed', { code: m.code }), content: deleteErrorText(e, t) }),
          })
        }
      >
        <Tooltip title={hint}>
          <Button
            size="small"
            danger
            icon={<DeleteOutlined />}
            disabled={!canDelete}
            loading={remove.isPending}
            aria-label={t('machine.deleteAria', { code: m.code })}
          />
        </Tooltip>
      </Popconfirm>
    </span>
  );
}

export function MachineActions({ m }: { m: Pick<Machine, 'code' | 'enabled'> }) {
  return (
    <Space size={8}>
      <MachineEnabledSwitch m={m} />
      <MachineDeleteButton m={m} />
    </Space>
  );
}
