// Тесты HTTP-клиента панели: разбор ошибок API, 204, сбой сети, событие 401.
import { afterEach, describe, expect, it, vi } from 'vitest';

import { ApiError, BAD_RESPONSE, NETWORK_ERROR, onUnauthorized, request } from '@/api/http';
import { mockFetch } from './test-utils';

// Подменяет fetch одним готовым ответом (тело - сырая строка, не обязательно JSON).
function stubRawResponse(status: number, body: string | null, statusText = '') {
  const fn = vi.fn(async () => new Response(body, { status, statusText }));
  vi.stubGlobal('fetch', fn);
  return fn;
}

// Ловит ошибку request() и проверяет, что это ApiError.
async function catchApiError(p: Promise<unknown>): Promise<ApiError> {
  try {
    await p;
  } catch (e) {
    expect(e).toBeInstanceOf(ApiError);
    return e as ApiError;
  }
  throw new Error('ожидалась ошибка ApiError, а запрос завершился успешно');
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('request - успешные ответы', () => {
  it('возвращает разобранный JSON', async () => {
    mockFetch(() => ({ body: { version: '1.2.3' } }));
    await expect(request('/api/version')).resolves.toEqual({ version: '1.2.3' });
  });

  it('204 без тела - undefined', async () => {
    stubRawResponse(204, null);
    await expect(request('/api/users/x', { method: 'DELETE' })).resolves.toBeUndefined();
  });

  it('200 с пустым телом - undefined', async () => {
    stubRawResponse(200, '');
    await expect(request('/api/x')).resolves.toBeUndefined();
  });

  it('200 с телом не JSON (HTML от чужого прокси) - ApiError bad_response, а не SyntaxError', async () => {
    stubRawResponse(200, '<!doctype html><html></html>');

    const err = await catchApiError(request('/api/users'));

    expect(err.status).toBe(200);
    expect(err.code).toBe(BAD_RESPONSE);
    expect(err.code).toBe('bad_response');
    expect(err.isNotMaster).toBe(false);
  });

  it('200 с телом не JSON не поднимает событие 401', async () => {
    stubRawResponse(200, 'not json');
    const listener = vi.fn();
    const off = onUnauthorized(listener);
    try {
      await catchApiError(request('/api/users'));
      expect(listener).not.toHaveBeenCalled();
    } finally {
      off();
    }
  });

  it('тело запроса сериализуется в JSON с Content-Type', async () => {
    const fetchFn = mockFetch(() => ({ body: { ok: true } }));
    await request('/api/users', { method: 'POST', body: { username: 'u1' } });

    const init = fetchFn.mock.calls[0][1] as RequestInit;
    expect(init.method).toBe('POST');
    expect(init.body).toBe(JSON.stringify({ username: 'u1' }));
    expect((init.headers as Record<string, string>)['Content-Type']).toBe('application/json');
  });

  it('GET без тела не шлёт Content-Type', async () => {
    const fetchFn = mockFetch(() => ({ body: {} }));
    await request('/api/role');

    const init = fetchFn.mock.calls[0][1] as RequestInit;
    expect(init.method).toBe('GET');
    expect(init.body).toBeUndefined();
    expect((init.headers as Record<string, string>)['Content-Type']).toBeUndefined();
  });
});

describe('request - разбор ошибок', () => {
  it.each([
    {
      name: 'новый формат {code, message}',
      status: 400,
      body: JSON.stringify({ code: 'bad_username', message: 'плохой логин' }),
      want: { code: 'bad_username', message: 'плохой логин' },
    },
    {
      name: 'старый формат {error} - текст в message, code пустой',
      status: 409,
      body: JSON.stringify({ error: 'эта нода — replica; запись идёт на master' }),
      want: { code: '', message: 'эта нода — replica; запись идёт на master' },
    },
    {
      name: 'message приоритетнее error',
      status: 400,
      body: JSON.stringify({ code: 'x', message: 'новое', error: 'старое' }),
      want: { code: 'x', message: 'новое' },
    },
    {
      name: 'тело не JSON - текст статуса',
      status: 502,
      body: '<html>Bad Gateway</html>',
      statusText: 'Bad Gateway',
      want: { code: '', message: 'Bad Gateway' },
    },
    {
      name: 'тело не JSON и нет statusText - «HTTP <код>»',
      status: 500,
      body: 'panic',
      want: { code: '', message: 'HTTP 500' },
    },
    {
      name: 'пустое тело',
      status: 503,
      body: null,
      want: { code: '', message: 'HTTP 503' },
    },
    {
      name: 'JSON не объект (строка) - текст статуса',
      status: 400,
      body: JSON.stringify('oops'),
      want: { code: '', message: 'HTTP 400' },
    },
    {
      name: 'поля не строки игнорируются',
      status: 400,
      body: JSON.stringify({ code: 42, message: { x: 1 } }),
      want: { code: '', message: 'HTTP 400' },
    },
  ])('$name', async ({ status, body, statusText, want }) => {
    stubRawResponse(status, body, statusText);

    const err = await catchApiError(request('/api/x'));

    expect(err.status).toBe(status);
    expect(err.code).toBe(want.code);
    expect(err.message).toBe(want.message);
  });

  it('сеть упала - ApiError со статусом 0 и code network', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))));

    const err = await catchApiError(request('/api/role'));

    expect(err.status).toBe(0);
    expect(err.code).toBe(NETWORK_ERROR);
    expect(err.message).toBe('Failed to fetch');
  });

  it('сеть упала без текста - запасное сообщение', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(new TypeError(''))));

    const err = await catchApiError(request('/api/role'));

    expect(err.code).toBe(NETWORK_ERROR);
    expect(err.message).toBe('network error');
  });

  it('отмена запроса (AbortError) пробрасывается как есть, не как network', async () => {
    const abort = new DOMException('aborted', 'AbortError');
    vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(abort)));

    await expect(request('/api/role')).rejects.toBe(abort);
  });
});

describe('request - событие 401', () => {
  it('на 401 вызываются все подписчики onUnauthorized', async () => {
    mockFetch(() => ({ status: 401, body: { code: 'unauthorized', message: 'нужен вход' } }));
    const a = vi.fn();
    const b = vi.fn();
    const offA = onUnauthorized(a);
    const offB = onUnauthorized(b);

    try {
      const err = await catchApiError(request('/api/users'));
      expect(err.status).toBe(401);
      expect(a).toHaveBeenCalledTimes(1);
      expect(b).toHaveBeenCalledTimes(1);
    } finally {
      offA();
      offB();
    }
  });

  it('при skipAuthRedirect подписчики не вызываются', async () => {
    mockFetch(() => ({ status: 401, body: { error: 'bad credentials' } }));
    const listener = vi.fn();
    const off = onUnauthorized(listener);

    try {
      await catchApiError(request('/api/login', { method: 'POST', body: {}, skipAuthRedirect: true }));
      expect(listener).not.toHaveBeenCalled();
    } finally {
      off();
    }
  });

  it('отписка работает', async () => {
    mockFetch(() => ({ status: 401, body: {} }));
    const listener = vi.fn();
    const off = onUnauthorized(listener);
    off();

    await catchApiError(request('/api/users'));

    expect(listener).not.toHaveBeenCalled();
  });

  it('на 403 и 500 подписчики не вызываются', async () => {
    const listener = vi.fn();
    const off = onUnauthorized(listener);
    try {
      for (const status of [403, 500]) {
        mockFetch(() => ({ status, body: {} }));
        await catchApiError(request('/api/users'));
      }
      expect(listener).not.toHaveBeenCalled();
    } finally {
      off();
    }
  });
});

describe('ApiError.isNotMaster', () => {
  it.each([
    { name: '409 + code not_master', status: 409, code: 'not_master', message: 'x', want: true },
    {
      name: '409 без кода, текст текущего бэкенда про replica/master',
      status: 409,
      code: '',
      message: 'эта нода — replica; запись идёт на master',
      want: true,
    },
    { name: '409 без кода, русский текст про мастер', status: 409, code: '', message: 'Пишите на Мастер', want: true },
    { name: '409 с другим кодом', status: 409, code: 'revision_conflict', message: 'ревизия устарела', want: false },
    {
      name: '409 с другим кодом, даже если в тексте есть master',
      status: 409,
      code: 'revision_conflict',
      message: 'master rev changed',
      want: false,
    },
    { name: '409 без кода с посторонним текстом', status: 409, code: '', message: 'already exists', want: false },
    { name: '400 + not_master', status: 400, code: 'not_master', message: 'x', want: false },
    { name: '400 с текстом про replica', status: 400, code: '', message: 'replica', want: false },
  ])('$name -> $want', ({ status, code, message, want }) => {
    expect(new ApiError(status, code, message).isNotMaster).toBe(want);
  });
});

describe('request - 409 от свежего бэкенда', () => {
  const MASTER = 'https://master.example.test:8080';

  it('запись на не-мастере: заголовок X-Telemux-Master и тело с code not_master', async () => {
    mockFetch(() => ({
      status: 409,
      headers: { 'X-Telemux-Master': MASTER },
      body: {
        code: 'not_master',
        message: 'узел в роли replica; запись принимает только мастер',
        error: 'not_master',
        master_url: MASTER,
      },
    }));

    const err = await catchApiError(request('/api/users', { method: 'POST', body: { username: 'u' } }));

    expect(err.status).toBe(409);
    expect(err.code).toBe('not_master');
    expect(err.message).toBe('узел в роли replica; запись принимает только мастер');
    expect(err.masterUrl).toBe(MASTER);
    expect(err.isNotMaster).toBe(true);
  });

  it('адрес мастера только в заголовке', async () => {
    mockFetch(() => ({ status: 409, headers: { 'X-Telemux-Master': MASTER }, body: { code: 'not_master', message: 'x' } }));

    const err = await catchApiError(request('/api/users', { method: 'POST', body: {} }));

    expect(err.masterUrl).toBe(MASTER);
  });

  it('адрес мастера только в поле master_url', async () => {
    mockFetch(() => ({ status: 409, body: { code: 'not_master', message: 'x', master_url: MASTER } }));

    const err = await catchApiError(request('/api/users', { method: 'POST', body: {} }));

    expect(err.masterUrl).toBe(MASTER);
  });

  it('заголовок приоритетнее поля master_url', async () => {
    mockFetch(() => ({
      status: 409,
      headers: { 'X-Telemux-Master': MASTER },
      body: { code: 'not_master', message: 'x', master_url: 'https://other.example.test' },
    }));

    const err = await catchApiError(request('/api/users', { method: 'POST', body: {} }));

    expect(err.masterUrl).toBe(MASTER);
  });

  it('заголовок есть, а тело не JSON - адрес мастера всё равно берётся', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response('conflict', { status: 409, headers: { 'X-Telemux-Master': MASTER } })),
    );

    const err = await catchApiError(request('/api/users', { method: 'POST', body: {} }));

    expect(err.masterUrl).toBe(MASTER);
  });

  it('адреса нет - masterUrl пустой', async () => {
    mockFetch(() => ({ status: 409, body: { code: 'not_master', message: 'x' } }));

    const err = await catchApiError(request('/api/users', { method: 'POST', body: {} }));

    expect(err.masterUrl).toBe('');
    expect(err.isNotMaster).toBe(true);
  });

  it('дубль пользователя (409 conflict) - это не «не мастер»', async () => {
    mockFetch(() => ({
      status: 409,
      body: { code: 'conflict', message: 'пользователь с таким username уже существует' },
    }));

    const err = await catchApiError(request('/api/users', { method: 'POST', body: { username: 'u' } }));

    expect(err.code).toBe('conflict');
    expect(err.message).toBe('пользователь с таким username уже существует');
    expect(err.isNotMaster).toBe(false);
    expect(err.masterUrl).toBe('');
  });

  it('ApiError без 4-го аргумента - masterUrl пустой', () => {
    expect(new ApiError(409, 'not_master', 'x').masterUrl).toBe('');
  });
});
