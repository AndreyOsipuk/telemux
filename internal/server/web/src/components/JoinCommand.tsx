// Одноразовая команда установки агента (JoinCommand из api/openapi.yaml): блок с
// командой и кнопкой «Скопировать», срок жизни, инструкция и статус файрвола на
// мастере. Плюс действие «Команда установки» для существующей машины (createJoin):
// выдаёт новую команду, прежняя неиспользованная при этом сгорает.

import { useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Alert, App, Button, Modal, Space, Tooltip, Typography } from 'antd';
import { CodeOutlined, CopyOutlined } from '@ant-design/icons';
import dayjs from 'dayjs';

import { useCreateJoin, useIsMaster } from '@/api/queries';
import type { FirewallStatus, JoinCommand } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import './JoinCommand.css';

const FIREWALL_ALERT: Record<Exclude<FirewallStatus, 'not_needed'>, 'success' | 'warning' | 'error'> = {
  opened: 'success',
  not_configured: 'warning',
  failed: 'error',
};

export function FirewallNote({ status }: { status?: FirewallStatus }) {
  const { t } = useTranslation();
  if (!status || status === 'not_needed') return null;
  return (
    <Alert
      type={FIREWALL_ALERT[status]}
      showIcon
      data-testid="join-firewall"
      data-firewall={status}
      title={t(`join.firewall.${status}`)}
    />
  );
}

// Выделить текст элемента - если буфер обмена недоступен (http без TLS, старый браузер),
// пользователь копирует сам через Ctrl+C.
function selectText(el: HTMLElement) {
  const sel = window.getSelection();
  if (!sel) return;
  const range = document.createRange();
  range.selectNodeContents(el);
  sel.removeAllRanges();
  sel.addRange(range);
}

export function JoinCommandView({ join, renewed = false }: { join: JoinCommand; renewed?: boolean }) {
  const { t } = useTranslation();
  const { message } = App.useApp();
  const ref = useRef<HTMLPreElement>(null);

  const copy = async () => {
    try {
      if (!navigator.clipboard?.writeText) throw new Error('clipboard unavailable');
      await navigator.clipboard.writeText(join.command);
      void message.success(t('join.copied'));
    } catch {
      if (ref.current) selectText(ref.current);
      void message.info(t('join.copyManually'));
    }
  };

  // Новый релей заводится без команды: ему некуда ходить, пока нет выхода с мостом.
  if (!join.command) {
    return (
      <Alert
        type="info"
        showIcon
        data-testid="join-no-command"
        message={t('join.noExitsTitle', { code: join.target })}
        description={t('join.noExits')}
      />
    );
  }

  return (
    <Space orientation="vertical" size={12} style={{ width: '100%' }} data-testid="join-command">
      <Typography.Text>{t('join.instruction')}</Typography.Text>
      <div className="join-command">
        <pre ref={ref} className="join-command-text" data-testid="join-command-text">
          {join.command}
        </pre>
        <Button icon={<CopyOutlined />} onClick={() => void copy()}>
          {t('join.copy')}
        </Button>
      </div>
      <Typography.Text type="secondary" data-testid="join-expires">
        {t('join.expires', { time: dayjs(join.expires_at).format('HH:mm') })}
      </Typography.Text>
      {renewed && <Typography.Text type="secondary">{t('join.renewedNote')}</Typography.Text>}
      <FirewallNote status={join.firewall} />
    </Space>
  );
}

// Действие «Команда установки» у машины в таблице: окно с предупреждением, что
// прежняя команда сгорит, затем - новая команда.
export function JoinCommandButton({ code, hasAgent }: { code: string; hasAgent: boolean }) {
  const { t } = useTranslation();
  const isMaster = useIsMaster();
  const [open, setOpen] = useState(false);
  const join = useCreateJoin();

  const close = () => {
    setOpen(false);
    join.reset();
  };

  // Окно рендерится порталом, но события React всплывают по дереву компонентов - до
  // строки таблицы, которая по клику открывает историю. Гасим их здесь.
  return (
    <span onClick={(e) => e.stopPropagation()}>
      <Tooltip title={isMaster ? undefined : t('role.writeOnMaster')}>
        <Button
          size="small"
          type={hasAgent ? 'default' : 'primary'}
          ghost={!hasAgent}
          icon={<CodeOutlined />}
          disabled={!isMaster}
          aria-label={t('join.actionOf', { code })}
          onClick={() => setOpen(true)}
        >
          {t('join.action')}
        </Button>
      </Tooltip>
      <Modal
        open={open}
        title={t('join.titleOf', { code })}
        onCancel={close}
        destroyOnHidden
        width={720}
        footer={
          join.data ? (
            <Button type="primary" onClick={close}>
              {t('common.close')}
            </Button>
          ) : (
            <Space>
              <Button onClick={close}>{t('common.cancel')}</Button>
              <Button type="primary" loading={join.isPending} onClick={() => join.mutate(code)}>
                {t('join.issue')}
              </Button>
            </Space>
          )
        }
      >
        {join.isError && <ErrorAlert error={join.error} title={t('join.failed')} />}
          {join.data ? (
            <JoinCommandView join={join.data} renewed />
          ) : (
            <Space orientation="vertical" size={8}>
              <Typography.Text>{t('join.renewWarning')}</Typography.Text>
              {hasAgent && <Typography.Text type="secondary">{t('join.hasAgentNote')}</Typography.Text>}
            </Space>
          )}
      </Modal>
    </span>
  );
}
