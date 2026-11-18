// Словари ru/en: одинаковый набор ключей, у русских плюралов есть все формы,
// и каждый ключ, который код передаёт в t('...'), существует.
import fs from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

import ru from '@/i18n/ru.json';
import en from '@/i18n/en.json';

type Dict = { [k: string]: string | Dict };
const PLURAL = /_(zero|one|two|few|many|other)$/;

function flatten(d: Dict, prefix = ''): string[] {
  return Object.entries(d).flatMap(([k, v]) => (typeof v === 'string' ? [prefix + k] : flatten(v, `${prefix}${k}.`)));
}
const base = (keys: string[]) => new Set(keys.map((k) => k.replace(PLURAL, '')));

const ruKeys = flatten(ru as Dict);
const enKeys = flatten(en as Dict);

function sourceFiles(dir: string): string[] {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) return e.name === 'test' ? [] : sourceFiles(p);
    return /\.tsx?$/.test(e.name) ? [p] : [];
  });
}

describe('словари i18n', () => {
  it('ru и en содержат одни и те же ключи', () => {
    expect([...base(ruKeys)].sort()).toEqual([...base(enKeys)].sort());
  });

  it('у русских плюралов есть формы one, few, many, other', () => {
    const plurals = [...new Set(ruKeys.filter((k) => PLURAL.test(k)).map((k) => k.replace(PLURAL, '')))];
    const missing = plurals.flatMap((b) =>
      ['one', 'few', 'many', 'other'].map((f) => `${b}_${f}`).filter((k) => !ruKeys.includes(k)),
    );
    expect(missing).toEqual([]);
  });

  it("каждый статический ключ t('...') из кода есть в словаре", () => {
    const known = base(ruKeys);
    const used = new Set<string>();
    for (const f of sourceFiles(path.resolve(import.meta.dirname, '..'))) {
      const src = fs.readFileSync(f, 'utf8');
      for (const m of src.matchAll(/\bt\(\s*'([a-zA-Z]+\.[a-zA-Z.]+)'/g)) used.add(m[1]);
    }
    expect(used.size).toBeGreaterThan(50);
    expect([...used].filter((k) => !known.has(k))).toEqual([]);
  });
});
