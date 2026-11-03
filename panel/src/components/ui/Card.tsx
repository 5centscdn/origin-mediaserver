'use client'

import React from 'react'
import clsx from 'clsx'

export function Card({
  className,
  children,
  ...rest
}: React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div className={clsx('rounded-lg border border-border bg-surface', className)} {...rest}>
      {children}
    </div>
  )
}

/** Card heading row: icon + title (+ description) on the left, actions on the right. */
export function CardHeader({
  title,
  description,
  icon,
  actions,
  className,
}: {
  title: React.ReactNode
  description?: React.ReactNode
  icon?: React.ReactNode
  actions?: React.ReactNode
  className?: string
}) {
  return (
    // Actions stay top-right at every width (no wrap), so Edit buttons line
    // up across cards; the title side shrinks and wraps instead.
    <div className={clsx('flex items-start justify-between gap-3 border-b border-border px-5 py-4', className)}>
      <div className="flex min-w-0 flex-1 items-start gap-3">
        {icon && <div className="mt-0.5 shrink-0 text-fg-muted">{icon}</div>}
        <div className="min-w-0">
          <h2 className="text-[15px] font-semibold leading-6 text-fg">{title}</h2>
          {description && <p className="mt-0.5 text-[13px] leading-5 text-fg-muted">{description}</p>}
        </div>
      </div>
      {actions && <div className="flex shrink-0 flex-wrap items-center justify-end gap-2">{actions}</div>}
    </div>
  )
}

export function CardBody({ className, children }: { className?: string; children: React.ReactNode }) {
  return <div className={clsx('px-5 py-4', className)}>{children}</div>
}

/** Label/value rows for read-only settings and details. */
export function DescriptionList({ children, className }: { children: React.ReactNode; className?: string }) {
  return <dl className={clsx('divide-y divide-border', className)}>{children}</dl>
}

export function DescriptionRow({
  label,
  children,
  hint,
  stacked,
}: {
  label: React.ReactNode
  children: React.ReactNode
  hint?: React.ReactNode
  /** Label above the value at every width (long values such as URL lists). */
  stacked?: boolean
}) {
  return (
    <div
      className={clsx(
        'grid grid-cols-1 gap-1.5 px-5 py-3',
        !stacked && 'sm:grid-cols-[minmax(150px,200px)_1fr] sm:items-baseline sm:gap-6',
      )}
    >
      <dt className="text-[13px] font-medium text-fg-secondary">
        {label}
        {hint && <div className="mt-0.5 text-xs font-normal text-fg-muted">{hint}</div>}
      </dt>
      <dd className="min-w-0 text-sm text-fg">{children}</dd>
    </div>
  )
}
