'use client'

import React, { useEffect, useState } from 'react'
import { useRouter, usePathname } from 'next/navigation'
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
} from 'lucide-react'
import clsx from 'clsx'
import {
  restoreAuth,
  getToken,
  logout,
  getActiveServer,
  setActiveServer,
  ServerEntry,
} from '@/lib/api'

interface NavItem {
  href: string
  label: string
  icon: React.ReactNode
}

const NAV: NavItem[] = [
  { href: '/', label: 'Dashboard', icon: <LayoutDashboard size={18} /> },
  { href: '/applications', label: 'Applications', icon: <AppWindow size={18} /> },
  { href: '/streams', label: 'Live Streams', icon: <Radio size={18} /> },
  { href: '/abr', label: 'ABR Groups', icon: <Layers size={18} /> },
  { href: '/dvr', label: 'DVR', icon: <HardDrive size={18} /> },
  { href: '/failover', label: 'Failover', icon: <ShieldAlert size={18} /> },
  { href: '/pull', label: 'Pull Sources', icon: <Download size={18} /> },
  { href: '/users', label: 'Users', icon: <Users size={18} /> },
  { href: '/servers', label: 'Servers', icon: <Server size={18} /> },
  { href: '/settings', label: 'Settings', icon: <Settings size={18} /> },
]

export function Layout({ children }: { children: React.ReactNode }) {
  const router = useRouter()
  const pathname = usePathname() ?? '/'
  const [ready, setReady] = useState(false)
  const [server, setServer] = useState<ServerEntry | null>(null)

  useEffect(() => {
    restoreAuth()
    const t = getToken()
    if (!t) {
      router.replace('/login')
      return
    }
    setServer(getActiveServer())
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
      <div className="min-h-screen flex items-center justify-center bg-gray-950 text-gray-400 text-sm">
        Loading…
      </div>
    )
  }

  const isActive = (href: string) => {
    if (href === '/') return pathname === '/' || pathname === ''
    return pathname === href || pathname.startsWith(href + '/')
  }

  return (
    <div className="flex h-screen bg-gray-950 text-gray-100 overflow-hidden">
      <aside className="w-60 flex-shrink-0 border-r border-gray-800 bg-gray-900 flex flex-col">
        <div className="px-4 py-4 border-b border-gray-800">
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img src="/logo.svg" alt="5centscdn" className="h-7 w-auto mb-2" />
          <div className="min-w-0">
            <div className="text-sm font-semibold text-white truncate">
              Origin Media Server
            </div>
            <div className="text-[10px] text-gray-500 uppercase tracking-wider">
              Control Panel
            </div>
          </div>
        </div>

        <nav className="flex-1 overflow-y-auto py-2">
          {NAV.map((item) => (
            <button
              key={item.href}
              type="button"
              onClick={() => router.push(item.href)}
              className={clsx(
                'w-full flex items-center gap-3 px-4 py-2 text-sm transition text-left',
                isActive(item.href)
                  ? 'bg-blue-600/15 text-blue-400 border-l-2 border-blue-500'
                  : 'text-gray-400 hover:bg-gray-800/60 hover:text-gray-200 border-l-2 border-transparent',
              )}
            >
              {item.icon}
              <span>{item.label}</span>
            </button>
          ))}
        </nav>

        <div className="border-t border-gray-800 p-3 space-y-2">
          <div className="px-2 py-1.5 rounded bg-gray-800/60 border border-gray-700/50">
            <div className="text-[10px] uppercase text-gray-500 tracking-wider mb-0.5">
              Active server
            </div>
            <div className="text-xs text-white font-medium truncate">
              {server ? server.name : 'This server (local)'}
            </div>
            <div className="text-[10px] text-gray-500 truncate">
              {server ? server.url : 'same origin'}
            </div>
            {server && (
              <button
                type="button"
                onClick={onResetServer}
                className="mt-1 text-[10px] text-blue-400 hover:text-blue-300"
              >
                Switch back to local
              </button>
            )}
          </div>
          <button
            type="button"
            onClick={onLogout}
            className="w-full flex items-center justify-center gap-2 px-3 py-2 text-sm rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
          >
            <LogOut size={14} />
            Logout
          </button>
        </div>
      </aside>

      <main className="flex-1 overflow-y-auto p-6">{children}</main>
    </div>
  )
}
