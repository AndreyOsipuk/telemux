// Источник: форк 3x-ui, frontend/src/hooks/useTheme.tsx (GPL-3.0). Адаптировано для
// telemux: только светлая и тёмная темы (без ultra-dark), по умолчанию - как в системе.

import { createContext, useCallback, useContext, useLayoutEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { theme as antdTheme } from 'antd';
import type { ThemeConfig } from 'antd';

const STORAGE_DARK = 'telemux:dark-mode';

function readInitialDark(): boolean {
  try {
    const raw = localStorage.getItem(STORAGE_DARK);
    if (raw !== null) return raw === 'true';
  } catch {
    // хранилище недоступно (приватный режим) - смотрим на систему
  }
  return typeof window.matchMedia === 'function'
    ? window.matchMedia('(prefers-color-scheme: dark)').matches
    : false;
}

function applyDom(isDark: boolean) {
  document.body.classList.remove('dark', 'light');
  document.body.classList.add(isDark ? 'dark' : 'light');
  // Нативные полосы прокрутки смотрят на color-scheme, а не на класс body.
  document.documentElement.style.colorScheme = isDark ? 'dark' : 'light';
}

const DARK_TOKENS = {
  colorBgBase: '#1a1b1f',
  colorBgLayout: '#1a1b1f',
  colorBgContainer: '#23252b',
  colorBgElevated: '#2d2f37',
};
const DARK_LAYOUT_TOKENS = {
  bodyBg: '#1a1b1f',
  headerBg: '#15161a',
  headerColor: '#ffffff',
  footerBg: '#1a1b1f',
  siderBg: '#15161a',
  triggerBg: '#23252b',
  triggerColor: '#ffffff',
};
const DARK_MENU_TOKENS = {
  darkItemBg: '#15161a',
  darkSubMenuItemBg: '#1a1b1f',
  darkPopupBg: '#23252b',
};
const DARK_CARD_TOKENS = { colorBorderSecondary: 'rgba(255, 255, 255, 0.06)' };
const LIGHT_CONTRAST_TOKENS = {
  colorTextDescription: 'rgba(0, 0, 0, 0.58)',
  colorTextTertiary: 'rgba(0, 0, 0, 0.58)',
  colorTextPlaceholder: '#767676',
  colorError: '#cf1322',
  colorErrorText: '#cf1322',
  colorSuccessText: '#237804',
};
const LIGHT_BUTTON_TOKENS = {
  colorPrimary: '#0958d9',
  colorPrimaryHover: '#2468e5',
  colorPrimaryActive: '#073ea8',
};

// hashed:false убирает обёртку :where(.css-<hash>) вокруг каждого правила antd,
// cssVar.key фиксирует область CSS-переменных (пояснения - в исходнике 3x-ui).
const SHARED_STYLE_CONFIG = { hashed: false, cssVar: { key: 'tmx' } } as const;

export function buildAntdThemeConfig(isDark: boolean): ThemeConfig {
  if (!isDark) {
    return {
      ...SHARED_STYLE_CONFIG,
      algorithm: antdTheme.defaultAlgorithm,
      token: LIGHT_CONTRAST_TOKENS,
      components: { Button: LIGHT_BUTTON_TOKENS },
    };
  }
  return {
    ...SHARED_STYLE_CONFIG,
    algorithm: antdTheme.darkAlgorithm,
    token: DARK_TOKENS,
    components: { Layout: DARK_LAYOUT_TOKENS, Menu: DARK_MENU_TOKENS, Card: DARK_CARD_TOKENS },
  };
}

interface ThemeContextValue {
  isDark: boolean;
  toggleTheme: () => void;
  antdThemeConfig: ThemeConfig;
}

const ThemeContext = createContext<ThemeContextValue | null>(null);

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [isDark, setIsDark] = useState<boolean>(readInitialDark);

  useLayoutEffect(() => {
    applyDom(isDark);
    try {
      localStorage.setItem(STORAGE_DARK, String(isDark));
    } catch {
      // не сохранилось - не страшно, тема останется до перезагрузки
    }
  }, [isDark]);

  const toggleTheme = useCallback(() => setIsDark((v) => !v), []);
  const antdThemeConfig = useMemo(() => buildAntdThemeConfig(isDark), [isDark]);
  const value = useMemo(
    () => ({ isDark, toggleTheme, antdThemeConfig }),
    [isDark, toggleTheme, antdThemeConfig],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error('useTheme: нужен <ThemeProvider>');
  return ctx;
}
