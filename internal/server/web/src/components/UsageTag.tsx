// Роль машины по серверам на экранах «Машины» и «Подключения»: выведенная или не
// используемая машина - серая строка с меткой; у выхода серверов «только через релей»
// счётчики подключений серые - клиенты подключаются к релею, а не к нему.

import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Tag, Tooltip, Typography } from 'antd';

import type { MachineUsage } from '@/utils/machineUsage';
import { idleUsage } from '@/utils/machineUsage';

export function UsageTag({ usage }: { usage?: MachineUsage }) {
  const { t } = useTranslation();
  if (usage !== 'retired' && usage !== 'unused') return null;
  return (
    <Tooltip title={t(`usage.hint.${usage}`)}>
      <Tag data-testid="usage-tag">{t(`usage.${usage}`)}</Tag>
    </Tooltip>
  );
}

// Счётчик подключений: на машине без клиентов - серый прочерк с объяснением.
export function IdleCount({ usage, children }: { usage?: MachineUsage; children: ReactNode }) {
  const { t } = useTranslation();
  if (!idleUsage(usage)) return <>{children}</>;
  return (
    <Tooltip title={t(`usage.hint.${usage}`)}>
      <Typography.Text type="secondary" data-testid="idle-count">
        -
      </Typography.Text>
    </Tooltip>
  );
}
