// i18n панели: ru (по умолчанию) и en. Словари маленькие - грузим оба сразу.

import i18next from 'i18next';
import { initReactI18next } from 'react-i18next';
import dayjs from 'dayjs';
import 'dayjs/locale/ru';

import ru from './ru.json';
import en from './en.json';

export type Lang = 'ru' | 'en';
const STORAGE_LANG = 'telemux:lang';

function readLang(): Lang {
  try {
    const v = localStorage.getItem(STORAGE_LANG);
    if (v === 'ru' || v === 'en') return v;
  } catch {
    // хранилище недоступно
  }
  return navigator.language?.toLowerCase().startsWith('en') ? 'en' : 'ru';
}

export function initI18n(lang: Lang = readLang()) {
  if (!i18next.isInitialized) {
    void i18next.use(initReactI18next).init({
      lng: lang,
      fallbackLng: 'ru',
      resources: { ru: { translation: ru }, en: { translation: en } },
      interpolation: { escapeValue: false },
      returnNull: false,
      initAsync: false,
    });
  }
  dayjs.locale(lang);
  document.documentElement.lang = lang;
  return i18next;
}

export function setLanguage(lang: Lang) {
  try {
    localStorage.setItem(STORAGE_LANG, lang);
  } catch {
    // не сохранилось - язык останется до перезагрузки
  }
  dayjs.locale(lang);
  document.documentElement.lang = lang;
  void i18next.changeLanguage(lang);
}

export { i18next as i18n };
