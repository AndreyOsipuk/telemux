// Значение с задержкой: поиск по мере набора не шлёт запрос на каждую букву.

import { useEffect, useState } from 'react';

export function useDebounced<T>(value: T, delayMs: number): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const id = window.setTimeout(() => setV(value), delayMs);
    return () => window.clearTimeout(id);
  }, [value, delayMs]);
  return v;
}
