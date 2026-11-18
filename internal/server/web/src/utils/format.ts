import dayjs from 'dayjs';

// Дата и время в одном формате на всех экранах, независимо от локали браузера.
export const formatDateTime = (iso: string) => dayjs(iso).format('DD.MM.YYYY HH:mm:ss');
export const formatDate = (iso: string) => dayjs(iso).format('DD.MM.YYYY');
