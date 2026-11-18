// HTTP-клиент панели. Ошибки API - обычный JSON {code, message} и HTTP-код
// (схема Error в api/openapi.yaml). Старый формат {error} без кода тоже понимаем:
// панель может открыться на узле со старым демоном.

import type { ErrorCode } from './types';

const NOT_MASTER: ErrorCode = 'not_master';

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  // Адрес мастера из 409 not_master (заголовок X-Telemux-Master или поле master_url).
  readonly masterUrl: string;

  constructor(status: number, code: string, message: string, masterUrl = '') {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.masterUrl = masterUrl;
  }

  // 409 при записи на реплику: мастер - другая машина.
  get isNotMaster(): boolean {
    if (this.status !== 409) return false;
    if (this.code === NOT_MASTER) return true;
    // Старый демон отвечал без кода - узнаём по тексту. С кодом текст не смотрим.
    return this.code === '' && /replica|master|мастер/i.test(this.message);
  }
}

// Коды клиента (их нет в спеке): сеть недоступна; ответ не JSON (на путь API
// ответил прокси или SPA-fallback страницей HTML).
export const NETWORK_ERROR = 'network';
export const BAD_RESPONSE = 'bad_response';

type UnauthorizedListener = () => void;
const unauthorizedListeners = new Set<UnauthorizedListener>();

// Подписка на 401 от любого запроса: панель показывает экран входа.
export function onUnauthorized(fn: UnauthorizedListener): () => void {
  unauthorizedListeners.add(fn);
  return () => unauthorizedListeners.delete(fn);
}

async function parseError(r: Response): Promise<ApiError> {
  let code = '';
  let message = r.statusText || `HTTP ${r.status}`;
  let masterUrl = r.headers.get('X-Telemux-Master') ?? '';
  try {
    const body: unknown = await r.json();
    if (body && typeof body === 'object') {
      const b = body as Record<string, unknown>;
      if (typeof b.code === 'string') code = b.code;
      if (typeof b.message === 'string') message = b.message;
      else if (typeof b.error === 'string') message = b.error;
      if (!masterUrl && typeof b.master_url === 'string') masterUrl = b.master_url;
    }
  } catch {
    // тело не JSON - оставляем текст статуса
  }
  return new ApiError(r.status, code, message, masterUrl);
}

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'DELETE';
  body?: unknown;
  signal?: AbortSignal;
  // Не поднимать событие «нужен вход» на 401 (сам вход отвечает 401 на неверный пароль).
  skipAuthRedirect?: boolean;
}

export async function request<T>(url: string, opts: RequestOptions = {}): Promise<T> {
  const init: RequestInit = {
    method: opts.method ?? 'GET',
    credentials: 'same-origin',
    signal: opts.signal,
    headers: { Accept: 'application/json' },
  };
  if (opts.body !== undefined) {
    init.headers = { ...init.headers, 'Content-Type': 'application/json' };
    init.body = JSON.stringify(opts.body);
  }

  let r: Response;
  try {
    r = await fetch(url, init);
  } catch (e) {
    if ((e as Error).name === 'AbortError') throw e;
    throw new ApiError(0, NETWORK_ERROR, (e as Error).message || 'network error');
  }

  if (!r.ok) {
    const err = await parseError(r);
    if (r.status === 401 && !opts.skipAuthRedirect) {
      unauthorizedListeners.forEach((fn) => fn());
    }
    throw err;
  }
  if (r.status === 204) return undefined as T;
  const text = await r.text();
  if (!text) return undefined as T;
  try {
    return JSON.parse(text) as T;
  } catch {
    throw new ApiError(r.status, BAD_RESPONSE, `ответ ${url} - не JSON`);
  }
}
