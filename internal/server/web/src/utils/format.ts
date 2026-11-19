import dayjs from 'dayjs';

// Дата и время в одном формате на всех экранах, независимо от локали браузера.
export const formatDateTime = (iso: string) => dayjs(iso).format('DD.MM.YYYY HH:mm:ss');
export const formatDate = (iso: string) => dayjs(iso).format('DD.MM.YYYY');

// Название продукта (движка) в интерфейсе: в API он `mtg`, а на машинах это mtg-multi.
export const productName = (p: string) => (p === 'mtg' ? 'mtg-multi' : p);

const BYTE_UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];

// Байты человекочитаемо, по 1024: 0 B, 512 B, 1.5 KB, 12 MB.
export function formatBytes(n: number | null | undefined): string {
  if (n === null || n === undefined || !Number.isFinite(n) || n < 0) return '-';
  let v = n;
  let i = 0;
  while (v >= 1024 && i < BYTE_UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  const s = i === 0 || v >= 100 ? String(Math.round(v)) : v.toFixed(1).replace(/\.0$/, '');
  return `${s} ${BYTE_UNITS[i]}`;
}

// Процент с одним знаком после запятой без лишнего нуля: 12%, 40.5%.
export function formatPct(v: number | null | undefined): string {
  if (v === null || v === undefined || !Number.isFinite(v)) return '-';
  return `${Number(v.toFixed(1))}%`;
}

// Целые дни до момента iso от now (отрицательные - уже прошёл).
export function daysUntil(iso: string, now = Date.now()): number {
  return Math.floor((Date.parse(iso) - now) / 86_400_000);
}
