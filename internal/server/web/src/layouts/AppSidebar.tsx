// Источник: форк 3x-ui, frontend/src/layouts/AppSidebar.tsx (GPL-3.0). Адаптировано
// для telemux: свой набор разделов, переключатели темы и языка, выход по /api/logout;
// убраны палитра команд, донат, спонсоры и подменю настроек.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ComponentType, CSSProperties } from 'react';
import { useLocation, useNavigate } from 'react-router';
import { useTranslation } from 'react-i18next';
import { Drawer, Layout, Menu } from 'antd';
import type { MenuProps } from 'antd';
import {
  ApiOutlined,
  BellOutlined,
  ClusterOutlined,
  CloudServerOutlined,
  DashboardOutlined,
  LinkOutlined,
  CloseOutlined,
  DeploymentUnitOutlined,
  LogoutOutlined,
  MenuOutlined,
  MoonOutlined,
  PushpinFilled,
  PushpinOutlined,
  SunOutlined,
  TeamOutlined,
} from '@ant-design/icons';

import { useTheme } from '@/hooks/useTheme';
import { useVersion } from '@/api/queries';
import { setLanguage } from '@/i18n';
import './AppSidebar.css';

const LOGOUT_KEY = '__logout__';
const RAIL_WIDTH = 72;
const SIDER_WIDTH = 220;
const SIDEBAR_PINNED_KEY = 'telemux:sidebar-pinned';

let hoveredAcrossRemounts = false;

interface NavItem {
  key: string;
  icon: ComponentType;
  title: string;
}

function readSidebarPinned() {
  try {
    // По умолчанию закреплён: разделов мало, подписи полезнее места.
    return localStorage.getItem(SIDEBAR_PINNED_KEY) !== 'false';
  } catch {
    return true;
  }
}

function saveSidebarPinned(pinned: boolean) {
  try {
    localStorage.setItem(SIDEBAR_PINNED_KEY, String(pinned));
  } catch {
    // не сохранилось - не страшно
  }
}

function ThemeButton({ id, isDark, onToggle, label }: { id: string; isDark: boolean; onToggle: () => void; label: string }) {
  return (
    <button id={id} type="button" className="sidebar-theme-cycle" aria-label={label} title={label} onClick={onToggle}>
      {isDark ? <MoonOutlined /> : <SunOutlined />}
    </button>
  );
}

function LangButton() {
  const { i18n, t } = useTranslation();
  const next = i18n.language === 'ru' ? 'en' : 'ru';
  return (
    <button
      type="button"
      className="sidebar-theme-cycle sidebar-lang"
      aria-label={t('menu.language')}
      title={t('menu.language')}
      onClick={() => setLanguage(next)}
    >
      {i18n.language === 'ru' ? 'RU' : 'EN'}
    </button>
  );
}

export interface AppSidebarProps {
  onLogout: () => void;
  authEnabled: boolean;
}

export default function AppSidebar({ onLogout, authEnabled }: AppSidebarProps) {
  const { t } = useTranslation();
  const { isDark, toggleTheme } = useTheme();
  const navigate = useNavigate();
  const { pathname } = useLocation();
  const { data: version } = useVersion();

  const [hovered, setHovered] = useState(() => hoveredAcrossRemounts);
  const [pinned, setPinned] = useState(readSidebarPinned);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const railCollapsed = !hovered && !pinned;
  const railStyle = useMemo(
    () => ({ '--sider-rail': `${pinned ? SIDER_WIDTH : RAIL_WIDTH}px` }) as CSSProperties,
    [pinned],
  );
  const rootRef = useRef<HTMLDivElement>(null);

  const updateHovered = useCallback((value: boolean) => {
    hoveredAcrossRemounts = value;
    setHovered(value);
  }, []);

  const togglePinned = useCallback(() => {
    const next = !pinned;
    saveSidebarPinned(next);
    setPinned(next);
  }, [pinned]);

  useEffect(() => {
    const timer = window.setTimeout(() => {
      const el = rootRef.current;
      if (el) updateHovered(el.matches(':hover'));
    }, 150);
    return () => window.clearTimeout(timer);
  }, [updateHovered]);

  const currentTheme: 'light' | 'dark' = isDark ? 'dark' : 'light';

  const navItems = useMemo<NavItem[]>(
    () => [
      { key: '/', icon: DashboardOutlined, title: t('menu.overview') },
      { key: '/servers', icon: CloudServerOutlined, title: t('menu.servers') },
      { key: '/map', icon: DeploymentUnitOutlined, title: t('menu.map') },
      { key: '/nodes', icon: ClusterOutlined, title: t('menu.nodes') },
      { key: '/connections', icon: LinkOutlined, title: t('menu.connections') },
      { key: '/users', icon: TeamOutlined, title: t('menu.users') },
      { key: '/events', icon: BellOutlined, title: t('menu.events') },
      { key: '/api-docs', icon: ApiOutlined, title: t('menu.apiDocs') },
    ],
    [t],
  );
  const utilItems = useMemo<NavItem[]>(
    () => (authEnabled ? [{ key: LOGOUT_KEY, icon: LogoutOutlined, title: t('menu.logout') }] : []),
    [authEnabled, t],
  );

  // «/» - только сам обзор: иначе startsWith('/') совпал бы с любым разделом.
  const selectedKey =
    navItems.find((n) => (n.key === '/' ? pathname === '/' : pathname.startsWith(n.key)))?.key ?? pathname;

  const toMenuItems = (items: NavItem[]): MenuProps['items'] =>
    items.map(({ key, icon: Icon, title }) => ({ key, icon: <Icon />, label: title, title: '' }));

  const onMenuClick = useCallback<NonNullable<MenuProps['onClick']>>(
    ({ key }) => {
      if (key === LOGOUT_KEY) {
        onLogout();
        return;
      }
      navigate(String(key));
    },
    [navigate, onLogout],
  );

  const versionLabel = version?.version ? `telemux ${version.version}` : '';

  return (
    <div
      ref={rootRef}
      className={`ant-sidebar${pinned ? ' sidebar-pinned' : ''}`}
      style={railStyle}
      onMouseEnter={() => updateHovered(true)}
      onMouseLeave={() => updateHovered(false)}
    >
      <Layout.Sider theme={currentTheme} width={SIDER_WIDTH} collapsedWidth={RAIL_WIDTH} collapsed={railCollapsed}>
        <div className="sider-brand">
          <div className="brand-block">
            <span className="brand-text">{railCollapsed ? 'tmx' : 'telemux'}</span>
          </div>
          {!railCollapsed && (
            <div className="brand-actions">
              <button
                type="button"
                className="sidebar-pin"
                aria-label={t('menu.pinSidebar')}
                aria-pressed={pinned}
                title={t(pinned ? 'menu.unpinSidebar' : 'menu.pinSidebar')}
                onClick={togglePinned}
              >
                {pinned ? <PushpinFilled /> : <PushpinOutlined />}
              </button>
              <LangButton />
              <ThemeButton id="theme-toggle" isDark={isDark} onToggle={toggleTheme} label={t('menu.theme')} />
            </div>
          )}
        </div>
        <Menu
          theme={currentTheme}
          mode="inline"
          selectedKeys={[selectedKey]}
          className="sider-nav"
          items={toMenuItems(navItems)}
          onClick={onMenuClick}
        />
        {utilItems.length > 0 && (
          <Menu
            theme={currentTheme}
            mode="inline"
            selectedKeys={[selectedKey]}
            className="sider-utility"
            items={toMenuItems(utilItems)}
            onClick={onMenuClick}
          />
        )}
        {versionLabel && (
          <div className="sider-footer">
            <span className="sider-version" title={versionLabel}>
              {railCollapsed ? 'v' : versionLabel}
            </span>
          </div>
        )}
      </Layout.Sider>

      <Drawer
        placement="left"
        closable={false}
        open={drawerOpen}
        rootClassName={currentTheme}
        size="min(82vw, 320px)"
        styles={{
          body: { padding: 0, display: 'flex', flexDirection: 'column', height: '100%' },
          header: { display: 'none' },
        }}
        onClose={() => setDrawerOpen(false)}
      >
        <div className="drawer-header">
          <div className="brand-block">
            <span className="drawer-brand">telemux</span>
          </div>
          <div className="drawer-header-actions">
            <LangButton />
            <ThemeButton id="theme-toggle-drawer" isDark={isDark} onToggle={toggleTheme} label={t('menu.theme')} />
            <button className="drawer-close" type="button" aria-label={t('common.close')} onClick={() => setDrawerOpen(false)}>
              <CloseOutlined />
            </button>
          </div>
        </div>
        <Menu
          theme={currentTheme}
          mode="inline"
          selectedKeys={[selectedKey]}
          className="drawer-menu drawer-nav"
          items={toMenuItems(navItems)}
          onClick={(info) => {
            onMenuClick(info);
            setDrawerOpen(false);
          }}
        />
        {utilItems.length > 0 && (
          <Menu
            theme={currentTheme}
            mode="inline"
            selectedKeys={[selectedKey]}
            className="drawer-menu drawer-utility"
            items={toMenuItems(utilItems)}
            onClick={(info) => {
              onMenuClick(info);
              setDrawerOpen(false);
            }}
          />
        )}
        {versionLabel && (
          <div className="drawer-footer">
            <span className="sider-version">{versionLabel}</span>
          </div>
        )}
      </Drawer>

      {!drawerOpen && (
        <button className="drawer-handle" type="button" aria-label={t('menu.openMenu')} onClick={() => setDrawerOpen(true)}>
          <MenuOutlined />
        </button>
      )}
    </div>
  );
}
