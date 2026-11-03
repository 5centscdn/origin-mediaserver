'use client'

import React from 'react'
import clsx from 'clsx'

/** Intentional empty state: icon, heading, one-line explanation, optional action. */
export function EmptyState({
  icon,
  title,
  description,
  action,
  className,
  compact,
}: {
  icon?: React.ReactNode
  title: React.ReactNode
  description?: React.ReactNode
  action?: React.ReactNode
  className?: string
  /** Smaller padding, for use inside a card or table. */
  compact?: boolean
}) {
  return (
    <div className={clsx('flex flex-col items-center justify-center text-center', compact ? 'px-6 py-10' : 'px-6 py-16', className)}>
      {icon && (
        <div className="mb-4 flex h-12 w-12 items-center justify-center rounded-full border border-border bg-elevated text-fg-muted">
          {icon}
        </div>
      )}
      <h3 className="text-[15px] font-semibold text-fg">{title}</h3>
      {description && <div className="mt-1.5 max-w-md text-[13px] leading-relaxed text-fg-muted">{description}</div>}
      {action && <div className="mt-5 flex flex-wrap items-center justify-center gap-2">{action}</div>}
    </div>
  )
}
