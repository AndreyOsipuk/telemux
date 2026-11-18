// Раскрывающаяся строка машины: последняя ошибка и история применений (apply-log).
// Образец - NodeHistoryPanel из форка 3x-ui; код написан заново.

import { useTranslation } from 'react-i18next';
import { Alert, Empty, Spin, Tag, Timeline, Typography } from 'antd';

import { useApplyLog } from '@/api/queries';
import type { ApplyResult, Target } from '@/api/types';
import { describeError } from '@/api/errors';
import { formatDateTime } from '@/utils/format';

const RESULT_COLOR: Record<ApplyResult, string> = {
  applied: 'green',
  pending: 'blue',
  rejected: 'red',
  rolled_back: 'orange',
  failed: 'red',
};

export default function TargetHistory({ target }: { target: Target }) {
  const { t } = useTranslation();
  const log = useApplyLog(target.code);

  return (
    <div style={{ padding: '4px 8px' }}>
      {target.last_error && (
        <Alert type="error" showIcon style={{ marginBottom: 12 }} title={t('pairs.lastError')} description={target.last_error} />
      )}
      <Typography.Text type="secondary">{t('pairs.history')}</Typography.Text>
      <div style={{ marginTop: 12 }}>
        {log.isPending ? (
          <Spin size="small" />
        ) : log.isError ? (
          <Typography.Text type="danger">{describeError(log.error, t)}</Typography.Text>
        ) : log.data.length === 0 ? (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t('pairs.noHistory')} />
        ) : (
          <Timeline
            items={log.data.map((e) => ({
              color: RESULT_COLOR[e.result],
              content: (
                <span>
                  <Typography.Text type="secondary">{formatDateTime(e.at)}</Typography.Text>{' '}
                  <Tag className="mono">rev {e.rev}</Tag>
                  <Tag color={RESULT_COLOR[e.result]}>{t(`applyResult.${e.result}`)}</Tag>
                  {e.message && <Typography.Text>{e.message}</Typography.Text>}
                </span>
              ),
            }))}
          />
        )}
      </div>
    </div>
  );
}
