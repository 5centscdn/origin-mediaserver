'use client'

import React from 'react'
import clsx from 'clsx'

export type StatusKind = 'live' | 'online' | 'success' | 'idle' | 'offline' | 'warning' | 'error' | 'info' | 'disabled'

const tone: Record<StatusKind, { dot: string; text: string; bg: string; label: string }> = {
  live: { dot: 'bg-success', text: 'text-success', bg: 'bg-success/10 border-success/30', label: 'Live' },
  online: { dot: 'bg-success', text: 'text-success', bg: 'bg-success/10 border-success/30', label: 'Online' },
  success: { dot: 'bg-success', text: 'text-success', bg: 'bg-success/10 border-success/30', label: 'Active' },
  idle: { dot: 'bg-fg-muted', text: 'text-fg-secondary', bg: 'bg-elevated border-border', label: 'Idle' },
  offline: { dot: 'bg-danger', text: 'text-danger', bg: 'bg-danger/10 border-danger/30', label: 'Offline' },
  warning: { dot: 'bg-warning', text: 'text-warning', bg: 'bg-warning/10 border-warning/30', label: 'Warning' },
  error: { dot: 'bg-danger', text: 'text-danger', bg: 'bg-danger/10 border-danger/30', label: 'Error' },
  info: { dot: 'bg-info', text: 'text-info', bg: 'bg-info/10 border-info/30', label: 'Info' },
  disabled: { dot: 'bg-fg-muted', text: 'text-fg-muted', bg: 'bg-elevated border-border', label: 'Disabled' },
}

/** Colored dot; pulses for "live". Pair with text — color alone isn't a status. */
export function StatusDot({ status, className }: { status: StatusKind; className?: string }) {
  const t = tone[status]
  return (
    <span className={clsx('relative inline-flex h-2 w-2 shrink-0', className)} aria-hidden>
      {status === 'live' && <span className={clsx('absolute inline-flex h-full w-full animate-ping rounded-full opacity-50', t.dot)} />}
      <span className={clsx('relative inline-flex h-2 w-2 rounded-full', t.dot)} />
    </span>
  )
}

/** Pill with dot + text, e.g. <StatusBadge status="live" /> or a custom label. */
export function StatusBadge({
  status,
  label,
  className,
}: {
  status: StatusKind
  label?: React.ReactNode
  className?: string
}) {
  const t = tone[status]
  return (
    <span
      className={clsx(
        'inline-flex items-center gap-1.5 whitespace-nowrap rounded-full border px-2 py-0.5 text-xs font-medium',
        t.bg,
        t.text,
        className,
      )}
    >
      <StatusDot status={status} />
      {label ?? t.label}
    </span>
  )
}
