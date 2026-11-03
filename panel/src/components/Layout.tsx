'use client'

import React, { useEffect, useState } from 'react'
import { useRouter, usePathname } from 'next/navigation'
import Link from 'next/link'
import {
  LayoutDashboard,
  Radio,
  Layers,
  HardDrive,
  ShieldAlert,
  Download,
  Users,
  Server,
  Settings,
  LogOut,
  AppWindow,
  ListVideo,
  AlertTriangle,
  Menu,
  X,
  PanelLeftClose,
  PanelLeftOpen,
  Undo2,
  type LucideIcon,
} from 'lucide-react'
import clsx from 'clsx'
import useSWR from 'swr'
import {
  restoreAuth,
  getToken,
  logout,
  getActiveServer,
  setActiveServer,
  ServerEntry,
  getNodeInfo,
} from '@/lib/api'
import { ToastProvider } from './ui/Toast'
import { StatusDot } from './ui/Status'
import { Tooltip } from './ui/Tooltip'

interface NavItem {
  href: string
  label: string
  icon: LucideIcon
}

const NAV: { group: string; items: NavItem[] }[] = [
  { group: 'Overview', items: [{ href: '/', label: 'Dashboard', icon: LayoutDashboard }] },
  {
    group: 'Streaming',
    items: [
      { href: '/applications', label: 'Applications', icon: AppWindow },
      { href: '/stream-configs', label: 'Streams', icon: ListVideo },
      { href: '/streams', label: 'Live Streams', icon: Radio },
      { href: '/abr', label: 'ABR Groups', icon: Layers },
      { href: '/dvr', label: 'DVR', icon: HardDrive },
      { href: '/failover', label: 'Failover', icon: ShieldAlert },
      { href: '/pull', label: 'Pull Sources', icon: Download },
    ],
  },
  { group: 'Infrastructure', items: [{ href: '/servers', label: 'Servers', icon: Server }] },
  {
    group: 'Administration',
    items: [
      { href: '/users', label: 'Users & Tokens', icon: Users },
      { href: '/settings', label: 'Settings', icon: Settings },
    ],
  },
]

const COLLAPSE_KEY = 'sidebarCollapsed'

export function Layout({ children }: { children: React.ReactNode }) {
  const router = useRouter()
  const pathname = usePathname() ?? '/'
  const [ready, setReady] = useState(false)
  const [server, setServer] = useState<ServerEntry | null>(null)
  const [collapsed, setCollapsed] = useState(false)
  const [drawer, setDrawer] = useState(false)
  // ≥1024px: the sidebar follows the user's expand/collapse choice.
  // 768–1023px: always the icon rail. <768px: hidden, opened as a drawer.
  const [isLg, setIsLg] = useState(true)
  // Animate the width only for the user's own expand/collapse click; a
  // breakpoint change (window resize) switches instantly, so the content
  // never shows mid-animation at the wrong width.
  const [animate, setAnimate] = useState(false)
  useEffect(() => {
    const mq = window.matchMedia('(min-width: 1024px)')
    const on = () => setIsLg(mq.matches)
    on()
    mq.addEventListener('change', on)
    return () => mq.removeEventListener('change', on)
  }, [])

  useEffect(() => {
    restoreAuth()
    const t = getToken()
    if (!t) {
      router.replace('/login')
      return
    }
    setServer(getActiveServer())
    try {
      setCollapsed(localStorage.getItem(COLLAPSE_KEY) === '1')
    } catch {}
    setReady(true)
    // React to server switches from other tabs / pages.
    const onStorage = (e: StorageEvent) => {
      if (e.key === 'activeServer' || e.key === 'activeServerUrl') {
        setServer(getActiveServer())
      }
    }
    window.addEventListener('storage', onStorage)
    return () => window.removeEventListener('storage', onStorage)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Close the mobile drawer on navigation.
  useEffect(() => setDrawer(false), [pathname])

  const toggleCollapsed = () => {
    setAnimate(true)
    setTimeout(() => setAnimate(false), 250)
    setCollapsed((c) => {
      try {
        localStorage.setItem(COLLAPSE_KEY, c ? '0' : '1')
      } catch {}
      return !c
    })
  }

  const onLogout = async () => {
    await logout()
    router.replace('/login')
  }

  const onResetServer = () => {
    setActiveServer(null)
    setServer(null)
    window.location.reload()
  }

  if (!ready) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-canvas text-sm text-fg-muted">
        <span className="inline-flex items-center gap-2">
          <span className="h-4 w-4 animate-spin rounded-full border-2 border-border-strong border-t-primary-fg" />
          Loading…
        </span>
      </div>
    )
  }

  const isActive = (href: string) => {
    if (href === '/') return pathname === '/' || pathname === ''
    return pathname === href || pathname.startsWith(href + '/')
  }

  const sidebar = (mini: boolean) => (
    <div className="flex h-full flex-col">
      <div className={clsx('flex h-16 shrink-0 items-center border-b border-border', mini ? 'justify-center px-2' : 'gap-3 px-5')}>
        {mini ? (
          // eslint-disable-next-line @next/next/no-img-element
          <img src="/logo.svg" alt="5centsCDN" className="h-6 w-6 object-cover object-left" />
        ) : (
          <div className="min-w-0">
            {/* eslint-disable-next-line @next/next/no-img-element */}
            <img src="/logo.svg" alt="5centsCDN" className="h-6 w-auto" />
            <div className="mt-1 truncate text-xs font-medium text-fg-secondary">Origin Media Server</div>
          </div>
        )}
      </div>

      <nav className="flex-1 overflow-y-auto px-2 py-3" aria-label="Main">
        {NAV.map((g, gi) => (
          <div key={g.group} className={clsx(gi > 0 && 'mt-4')}>
            {mini ? (
              gi > 0 && <div className="mx-2 mb-3 h-px bg-border" />
            ) : (
              <div className="mb-1 px-3 text-xs font-semibold uppercase tracking-wider text-fg-muted">
                {g.group}
              </div>
            )}
            <ul className={clsx('space-y-0.5', mini && 'flex flex-col items-center')}>
              {g.items.map((item) => {
                const active = isActive(item.href)
                const Icon = item.icon
                return (
                  <li key={item.href}>
                    <Tooltip content={item.label} side="right" disabled={!mini}>
                    <Link
                      href={item.href}
                      aria-label={mini ? item.label : undefined}
                      aria-current={active ? 'page' : undefined}
                      className={clsx(
                        'group relative flex items-center rounded-md text-[13px] font-medium transition-colors',
                        mini ? 'h-9 w-12 justify-center' : 'h-9 w-full gap-3 px-3',
                        active
                          ? 'bg-primary/15 text-fg'
                          : 'text-fg-secondary hover:bg-elevated hover:text-fg',
                      )}
                    >
                      {active && <span className="absolute inset-y-1.5 left-0 w-0.5 rounded-full bg-primary-fg" />}
                      <Icon size={17} className={clsx('shrink-0', active ? 'text-primary-fg' : 'text-fg-muted group-hover:text-fg-secondary')} />
                      {!mini && <span className="truncate">{item.label}</span>}
                    </Link>
                    </Tooltip>
                  </li>
                )
              })}
            </ul>
          </div>
        ))}
      </nav>

      <div className={clsx('shrink-0 space-y-2 border-t border-border', mini ? 'p-2' : 'p-3')}>
        {mini ? (
          <div className="flex justify-center py-1">
            <Tooltip content={server ? `Active server: ${server.name}` : 'Active server: this server'} side="right">
              <span tabIndex={0} aria-label={server ? `Active server: ${server.name}` : 'Active server: this server'} className="rounded p-1">
                <StatusDot status={server ? 'warning' : 'online'} />
              </span>
            </Tooltip>
          </div>
        ) : (
          <div className="rounded-md border border-border bg-surface px-3 py-2.5">
            <div className="flex items-center gap-2">
              <StatusDot status={server ? 'warning' : 'online'} />
              <span className="text-xs font-medium uppercase tracking-wider text-fg-muted">Active server</span>
            </div>
            <div className="mt-1 truncate text-[13px] font-medium text-fg" title={server?.name}>
              {server ? server.name : 'This server'}
            </div>
            <div className="truncate font-mono text-xs text-fg-muted" title={server?.url}>
              {server ? server.url : 'local · same origin'}
            </div>
            {server && (
              <button
                type="button"
                onClick={onResetServer}
                className="mt-1.5 inline-flex items-center gap-1 text-xs font-medium text-primary-fg hover:text-fg"
              >
                <Undo2 size={12} />
                Switch back to local
              </button>
            )}
          </div>
        )}
        <div className={clsx('flex gap-1', mini && 'flex-col items-center')}>
          <Tooltip content="Log out" side="right" disabled={!mini}>
            <button
              type="button"
              onClick={onLogout}
              aria-label={mini ? 'Log out' : undefined}
              className={clsx(
                'flex h-8 items-center justify-center gap-2 rounded-md text-[13px] text-fg-secondary transition-colors hover:bg-elevated hover:text-fg',
                mini ? 'w-12' : 'flex-1 px-3',
              )}
            >
              <LogOut size={15} />
              {!mini && 'Log out'}
            </button>
          </Tooltip>
          {isLg && (
            <Tooltip content={mini ? 'Expand sidebar' : 'Collapse sidebar'} side={mini ? 'right' : 'top'}>
              <button
                type="button"
                onClick={toggleCollapsed}
                aria-label={mini ? 'Expand sidebar' : 'Collapse sidebar'}
                aria-expanded={!mini}
                className={clsx(
                  'flex h-8 items-center justify-center rounded-md text-fg-muted transition-colors hover:bg-elevated hover:text-fg',
                  mini ? 'w-12' : 'w-8',
                )}
              >
                {mini ? <PanelLeftOpen size={15} /> : <PanelLeftClose size={15} />}
              </button>
            </Tooltip>
          )}
        </div>
      </div>
    </div>
  )

  return (
    <ToastProvider>
      <div className="flex h-screen overflow-hidden bg-canvas text-fg">
        {/* Desktop sidebar */}
        <aside
          className={clsx(
            'hidden shrink-0 border-r border-border bg-sidebar md:block',
            animate && 'transition-[width] duration-200',
            !isLg || collapsed ? 'w-16' : 'w-[248px]',
          )}
        >
          {sidebar(!isLg || collapsed)}
        </aside>

        {/* Mobile drawer */}
        {drawer && (
          <div className="fixed inset-0 z-40 md:hidden" role="dialog" aria-modal="true" aria-label="Navigation">
            <div className="absolute inset-0 bg-black/60" onClick={() => setDrawer(false)} />
            <aside className="absolute inset-y-0 left-0 w-[264px] max-w-[85vw] border-r border-border bg-sidebar shadow-pop">
              <button
                type="button"
                onClick={() => setDrawer(false)}
                aria-label="Close navigation"
                className="absolute right-2 top-4 rounded-md p-1.5 text-fg-muted hover:bg-elevated hover:text-fg"
              >
                <X size={18} />
              </button>
              {sidebar(false)}
            </aside>
          </div>
        )}

        <div className="flex min-w-0 flex-1 flex-col">
          {/* Mobile top bar */}
          <header className="flex h-14 shrink-0 items-center gap-3 border-b border-border bg-sidebar px-4 md:hidden">
            <button
              type="button"
              onClick={() => setDrawer(true)}
              aria-label="Open navigation"
              className="-ml-1.5 rounded-md p-1.5 text-fg-secondary hover:bg-elevated hover:text-fg"
            >
              <Menu size={20} />
            </button>
            {/* eslint-disable-next-line @next/next/no-img-element */}
            <img src="/logo.svg" alt="5centsCDN" className="h-5 w-auto" />
            <span className="truncate text-[13px] font-medium text-fg-secondary">Origin Media Server</span>
          </header>

          <main className="min-w-0 flex-1 overflow-y-auto">
            <div className="mx-auto w-full max-w-[1680px] space-y-6 px-4 py-6 sm:px-6 lg:px-8 lg:py-8">
              <LiveCacheBanner />
              {children}
            </div>
          </main>
        </div>
      </div>
    </ToastProvider>
  )
}

// LiveCacheBanner warns on every page while memory is short: live
// HLS/DASH output has moved from RAM to disk (hybrid mode), or can't
// (memory mode). See internal/livestore.
function LiveCacheBanner() {
  const { data: node } = useSWR('node', getNodeInfo, { refreshInterval: 15000 })
  const lc = node?.live_cache
  if (!lc?.warning) return null
  return (
    <div role="alert" className="flex items-start gap-3 rounded-lg border border-warning/40 bg-warning/10 px-4 py-3 text-sm text-warning">
      <AlertTriangle size={16} className="mt-0.5 shrink-0" />
      <span className="text-fg">
        <span className="font-semibold text-warning">Memory warning: </span>
        {lc.warning}
      </span>
    </div>
  )
}
