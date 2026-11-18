// Статус синхронизации пользователей на ноды (старый push-синк) и запуск вручную.

import { useTranslation } from 'react-i18next';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Alert, App, Button, Card, Col, Descriptions, Row, Statistic, Tag, Tooltip } from 'antd';
import { ReloadOutlined, SyncOutlined } from '@ant-design/icons';

import { api } from '@/api/client';
import { describeError } from '@/api/errors';
import { qk, useIsMaster, useRole, useSyncStatus } from '@/api/queries';
import ErrorAlert from '@/components/ErrorAlert';
import { formatDateTime } from '@/utils/format';

// Нулевое время Go (0001-01-01) - синка ещё не было.
const isZeroTime = (s?: string) => !s || s.startsWith('0001');

export default function SyncPage() {
  const { t } = useTranslation();
  const { message } = App.useApp();
  const qc = useQueryClient();
  const sync = useSyncStatus();
  const role = useRole();
  const isMaster = useIsMaster();

  const run = useMutation({
    mutationFn: api.syncNow,
    onSuccess: (st) => {
      qc.setQueryData(qk.sync, st);
      if (st.error) message.warning(st.error);
      else message.success(t('sync.done'));
    },
    onError: (e) => message.error(describeError(e, t)),
  });

  const st = sync.data;
  return (
    <>
      <div className="page-header">
        <h1>{t('menu.sync')}</h1>
        <div style={{ display: 'flex', gap: 8 }}>
          <Button icon={<ReloadOutlined />} onClick={() => sync.refetch()} loading={sync.isFetching}>
            {t('common.refresh')}
          </Button>
          <Tooltip title={isMaster ? undefined : t('role.writeOnMaster')}>
            <Button type="primary" icon={<SyncOutlined />} loading={run.isPending} disabled={!isMaster} onClick={() => run.mutate()}>
              {t('sync.runNow')}
            </Button>
          </Tooltip>
        </div>
      </div>
      {sync.isError && <ErrorAlert error={sync.error} onRetry={() => sync.refetch()} />}
      {st?.error && <Alert type="error" showIcon style={{ marginBottom: 16 }} title={t('sync.lastError')} description={st.error} />}
      <Row gutter={[16, 16]}>
        <Col xs={12} md={6}>
          <Card><Statistic title={t('sync.role')} value={role.data?.role ?? '-'} /></Card>
        </Col>
        <Col xs={12} md={6}>
          <Card><Statistic title={t('sync.creates')} value={st?.creates ?? '-'} /></Card>
        </Col>
        <Col xs={12} md={6}>
          <Card><Statistic title={t('sync.patches')} value={st?.patches ?? '-'} /></Card>
        </Col>
        <Col xs={12} md={6}>
          <Card><Statistic title={t('sync.deletes')} value={st?.deletes ?? '-'} /></Card>
        </Col>
      </Row>
      <Card style={{ marginTop: 16 }} loading={sync.isPending}>
        <Descriptions column={{ xs: 1, md: 2 }} size="small">
          <Descriptions.Item label={t('sync.at')}>
            {isZeroTime(st?.at) ? t('sync.never') : formatDateTime(st!.at)}
          </Descriptions.Item>
          <Descriptions.Item label={t('sync.mode')}>{st?.mode || '-'}</Descriptions.Item>
          <Descriptions.Item label={t('sync.applied')}>{st?.applied ?? '-'}</Descriptions.Item>
          <Descriptions.Item label={t('sync.failed')}>
            {st?.failed ? <Tag color="error">{st.failed}</Tag> : (st?.failed ?? '-')}
          </Descriptions.Item>
          <Descriptions.Item label={t('sync.aborted')}>
            {st?.aborted ? <Tag color="warning">{t('common.yes')}</Tag> : t('common.no')}
          </Descriptions.Item>
        </Descriptions>
      </Card>
    </>
  );
}
