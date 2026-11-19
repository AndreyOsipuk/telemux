// «Сертификаты»: WEB-домены включённых релеев и состояние их сертификатов на мастере.
// Меньше 14 дней до конца срока (или уже истёк) - строка подсвечена.

import { useTranslation } from 'react-i18next';
import { useIsFetching, useQueryClient } from '@tanstack/react-query';
import { Button, Card, Typography } from 'antd';
import { ReloadOutlined } from '@ant-design/icons';

import CertsTable, { CERT_WARN_DAYS } from '@/components/CertsTable';

export default function CertsPage() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const fetching = useIsFetching({ queryKey: ['certs'] });
  return (
    <>
      <div className="page-header">
        <h1>{t('menu.certs')}</h1>
        <Button icon={<ReloadOutlined />} onClick={() => void qc.invalidateQueries({ queryKey: ['certs'] })} loading={fetching > 0}>
          {t('common.refresh')}
        </Button>
      </div>
      <Card extra={<Typography.Text type="secondary">{t('certs.hint', { days: CERT_WARN_DAYS })}</Typography.Text>} title={t('certs.title')}>
        <CertsTable />
      </Card>
    </>
  );
}
