'use client'

import React from 'react'
import clsx from 'clsx'

interface StatCardProps {
  label: string
  value: string | number
  sub?: React.ReactNode
  icon?: React.ReactNode
  color?: 'green' | 'blue' | 'purple' | 'yellow' | 'red'
}

// KPI card: label, large value, icon chip with a colored accent.
const accent = {
  green: 'bg-success/10 text-success ring-success/20',
  blue: 'bg-info/10 text-info ring-info/20',
  purple: 'bg-primary/15 text-primary-fg ring-primary/25',
  yellow: 'bg-warning/10 text-warning ring-warning/20',
  red: 'bg-danger/10 text-danger ring-danger/20',
}

export function StatCard({ label, value, sub, icon, color = 'blue' }: StatCardProps) {
  return (
    <div className="rounded-lg border border-border bg-surface p-4 sm:p-5">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="text-[13px] font-medium text-fg-muted">{label}</div>
          <div className="tabular mt-2 text-[22px] font-semibold sm:text-[28px] leading-none tracking-tight text-fg">{value}</div>
          {sub && <div className="mt-2 text-xs text-fg-muted">{sub}</div>}
        </div>
        {icon && (
          <div className={clsx('hidden h-9 w-9 shrink-0 items-center justify-center rounded-md ring-1 sm:flex', accent[color])}>
            {icon}
          </div>
        )}
      </div>
    </div>
  )
}
