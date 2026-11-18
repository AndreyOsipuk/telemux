// Понятный текст ошибки API для пользователя панели.

import type { TFunction } from 'i18next';

import { ApiError, BAD_RESPONSE, NETWORK_ERROR } from './http';

export function describeError(err: unknown, t: TFunction): string {
  if (err instanceof ApiError) {
    if (err.isNotMaster) {
      return err.masterUrl ? t('errors.notMasterAt', { url: err.masterUrl }) : t('errors.notMaster');
    }
    if (err.code === NETWORK_ERROR) return t('errors.network');
    if (err.code === BAD_RESPONSE) return t('errors.badResponse');
    switch (err.status) {
      case 401:
        return t('errors.unauthorized');
      case 403:
        return t('errors.forbidden');
      case 404:
        return err.message && err.message !== 'Not Found' ? err.message : t('errors.notFound');
      case 503:
        return t('errors.unavailable');
    }
    if (err.status >= 500) return t('errors.server', { status: err.status });
    return err.message;
  }
  if (err instanceof Error) return err.message;
  return String(err);
}
