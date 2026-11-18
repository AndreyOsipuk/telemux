// Источник: форк 3x-ui, frontend/src/pages/nodes/NodeList.tsx, useRelativeTime (GPL-3.0).
// Адаптировано: ISO-время вместо unix-секунд, тикающие «сейчас», подписи из i18n.

import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';

// Текущее время, обновляемое раз в tickMs: «видели N секунд назад» не застывает.
export function useNow(tickMs = 5_000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), tickMs);
    return () => window.clearInterval(id);
  }, [tickMs]);
  return now;
}

export function secondsSince(iso: string | null | undefined, now: number): number | null {
  if (!iso) return null;
  const ts = Date.parse(iso);
  if (Number.isNaN(ts)) return null;
  return Math.max(0, Math.floor((now - ts) / 1000));
}

export function useRelativeTime() {
  const { t } = useTranslation();
  return useMemo(
    () => (diffSec: number | null) => {
      if (diffSec === null) return t('time.never');
      if (diffSec < 5) return t('time.justNow');
      if (diffSec < 60) return t('time.secondsAgo', { count: diffSec });
      if (diffSec < 3600) return t('time.minutesAgo', { count: Math.floor(diffSec / 60) });
      if (diffSec < 86400) return t('time.hoursAgo', { count: Math.floor(diffSec / 3600) });
      return t('time.daysAgo', { count: Math.floor(diffSec / 86400) });
    },
    [t],
  );
}

// Длительность без «назад»: «12 мин», «3 ч».
export function useDuration() {
  const { t } = useTranslation();
  return useMemo(
    () => (sec: number) => {
      if (sec < 60) return t('time.seconds', { count: sec });
      if (sec < 3600) return t('time.minutes', { count: Math.floor(sec / 60) });
      if (sec < 86400) return t('time.hours', { count: Math.floor(sec / 3600) });
      return t('time.days', { count: Math.floor(sec / 86400) });
    },
    [t],
  );
}
