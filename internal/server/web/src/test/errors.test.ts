// Тесты describeError: понятный текст ошибки API для пользователя.
import { describe, expect, it } from 'vitest';

import { describeError } from '@/api/errors';
import { ApiError, BAD_RESPONSE, NETWORK_ERROR } from '@/api/http';
import { i18n } from '@/i18n';
import ru from '@/i18n/ru.json';

const t = i18n.t.bind(i18n);

describe('describeError', () => {
  it.each([
    {
      name: '409 not_master',
      err: new ApiError(409, 'not_master', 'x'),
      want: ru.errors.notMaster,
    },
    {
      name: '409 старого формата про replica',
      err: new ApiError(409, '', 'эта нода — replica; запись идёт на master'),
      want: ru.errors.notMaster,
    },
    {
      name: '409 not_master с адресом мастера - текст со ссылкой',
      err: new ApiError(409, 'not_master', 'узел в роли replica', 'https://master.example.test'),
      want: 'Эта машина - не мастер. Изменения принимает только мастер: https://master.example.test',
    },
    {
      name: '409 старого формата с адресом мастера',
      err: new ApiError(409, '', 'эта нода — replica; запись идёт на master', 'https://master.example.test'),
      want: 'Эта машина - не мастер. Изменения принимает только мастер: https://master.example.test',
    },
    {
      name: '409 not_master с пустым адресом - общий текст',
      err: new ApiError(409, 'not_master', 'x', ''),
      want: ru.errors.notMaster,
    },
    {
      name: '409 дубль пользователя (conflict) - текст сервера, не «не мастер»',
      err: new ApiError(409, 'conflict', 'пользователь с таким username уже существует'),
      want: 'пользователь с таким username уже существует',
    },
    {
      name: '409 conflict с адресом мастера всё равно не «не мастер»',
      err: new ApiError(409, 'conflict', 'дубль', 'https://master.example.test'),
      want: 'дубль',
    },
    {
      name: '409 другой конфликт - текст сервера',
      err: new ApiError(409, 'revision_conflict', 'ревизия устарела'),
      want: 'ревизия устарела',
    },
    {
      name: 'сеть',
      err: new ApiError(0, NETWORK_ERROR, 'Failed to fetch'),
      want: ru.errors.network,
    },
    {
      name: 'ответ не JSON',
      err: new ApiError(200, BAD_RESPONSE, 'ответ /api/users - не JSON'),
      want: ru.errors.badResponse,
    },
    { name: '401', err: new ApiError(401, '', 'x'), want: ru.errors.unauthorized },
    { name: '403', err: new ApiError(403, '', 'x'), want: ru.errors.forbidden },
    { name: '404 без текста', err: new ApiError(404, 'not_found', 'Not Found'), want: ru.errors.notFound },
    { name: '404 с текстом сервера', err: new ApiError(404, '', 'нет пользователя u1'), want: 'нет пользователя u1' },
    { name: '503', err: new ApiError(503, '', 'db down'), want: ru.errors.unavailable },
    {
      name: '500 - со статусом',
      err: new ApiError(500, '', 'sql: no rows'),
      want: 'Ошибка сервера (500). Подробности - в журнале демона.',
    },
    {
      name: '502 - со статусом',
      err: new ApiError(502, '', 'Bad Gateway'),
      want: 'Ошибка сервера (502). Подробности - в журнале демона.',
    },
    { name: '400 - текст сервера', err: new ApiError(400, 'bad_username', 'плохой логин'), want: 'плохой логин' },
    { name: 'обычная Error', err: new Error('boom'), want: 'boom' },
    { name: 'строка', err: 'просто строка', want: 'просто строка' },
  ])('$name', ({ err, want }) => {
    expect(describeError(err, t)).toBe(want);
  });

  it('500 не раскрывает текст сервера', () => {
    const text = describeError(new ApiError(500, '', 'pq: relation "users" does not exist'), t);
    expect(text).not.toContain('relation');
  });
});
