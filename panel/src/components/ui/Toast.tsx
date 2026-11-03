'use client'

import React, { createContext, useCallback, useContext, useState } from 'react'
import clsx from 'clsx'
import { CheckCircle2, AlertTriangle, Info, X } from 'lucide-react'

type ToastKind = 'success' | 'error' | 'info'
interface ToastItem {
  id: number
  kind: ToastKind
  message: string
}

interface ToastApi {
  success: (message: string) => void
  error: (message: string) => void
  info: (message: string) => void
}

const noop: ToastApi = { success: () => {}, error: () => {}, info: () => {} }
const ToastContext = createContext<ToastApi>(noop)

/** toast.success('Saved') etc. Safe to call outside a provider (no-op). */
export function useToast(): ToastApi {
  return useContext(ToastContext)
}

let nextId = 1

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([])

  const dismiss = useCallback((id: number) => setItems((l) => l.filter((t) => t.id !== id)), [])
  const push = useCallback(
    (kind: ToastKind, message: string) => {
      const id = nextId++
      setItems((l) => [...l.slice(-3), { id, kind, message }])
      setTimeout(() => dismiss(id), kind === 'error' ? 6000 : 3000)
    },
    [dismiss],
  )
  const [api] = useState<ToastApi>(() => ({
    success: (m) => push('success', m),
    error: (m) => push('error', m),
    info: (m) => push('info', m),
  }))

  return (
    <ToastContext.Provider value={api}>
      {children}
      <div
        aria-live="polite"
        className="pointer-events-none fixed bottom-4 right-4 z-[60] flex w-[calc(100%-2rem)] max-w-sm flex-col gap-2"
      >
        {items.map((t) => {
          const Icon = t.kind === 'success' ? CheckCircle2 : t.kind === 'error' ? AlertTriangle : Info
          return (
            <div
              key={t.id}
              role={t.kind === 'error' ? 'alert' : 'status'}
              className="pointer-events-auto flex animate-toast-in items-start gap-3 rounded-lg border border-border bg-elevated px-4 py-3 text-sm text-fg shadow-pop"
            >
              <Icon
                size={16}
                className={clsx(
                  'mt-0.5 shrink-0',
                  t.kind === 'success' ? 'text-success' : t.kind === 'error' ? 'text-danger' : 'text-info',
                )}
              />
              <span className="min-w-0 flex-1 break-words">{t.message}</span>
              <button
                type="button"
                onClick={() => dismiss(t.id)}
                className="shrink-0 text-fg-muted transition-colors hover:text-fg"
                aria-label="Dismiss"
              >
                <X size={14} />
              </button>
            </div>
          )
        })}
      </div>
    </ToastContext.Provider>
  )
}
