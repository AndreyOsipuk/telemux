import { Alert, Typography } from 'antd';
import { useTranslation } from 'react-i18next';

import { describeError } from '@/api/errors';
import { useRole } from '@/api/queries';

// Панель открыта не на мастере: запись вернёт 409, предупреждаем заранее и,
// если демон знает мастера, даём ссылку на его панель.
export default function NotMasterBanner() {
  const { t } = useTranslation();
  const { data, error } = useRole();
  if (error && !data) {
    return (
      <Alert
        type="warning"
        showIcon
        banner
        style={{ marginBottom: 16 }}
        title={t('role.unknownBanner')}
        description={describeError(error, t)}
      />
    );
  }
  if (!data || data.is_master) return null;
  const url = data.master_url;
  return (
    <Alert
      type="warning"
      showIcon
      banner
      style={{ marginBottom: 16 }}
      title={
        url ? (
          <span>
            {t('role.replicaBannerAt', { role: data.role, url: '' })}
            <Typography.Link href={url}>{url}</Typography.Link>
          </span>
        ) : (
          t('role.replicaBanner', { role: data.role })
        )
      }
      description={data.reason || undefined}
    />
  );
}
