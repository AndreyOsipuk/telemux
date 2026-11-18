// Источник: форк 3x-ui, frontend/src/main.tsx, восстановление после деплоя (GPL-3.0).
// Вкладка, открытая до обновления бинаря, просит старые чанки и получает 404
// (см. ../web.go). Перезагружаем страницу один раз на версию бандла; повторная
// ошибка той же версии - настоящая авария, в цикл не уходим.

const KEY = `telemux:chunk-recovery:${import.meta.url}`;
let committed = false;

export function installChunkRecovery() {
  window.addEventListener('vite:preloadError', (event) => {
    if (committed) return;
    committed = true;
    try {
      if (sessionStorage.getItem(KEY) != null) {
        committed = false;
        return;
      }
      sessionStorage.setItem(KEY, '1');
    } catch {
      committed = false;
      return;
    }
    event.preventDefault();
    location.reload();
  });
}
