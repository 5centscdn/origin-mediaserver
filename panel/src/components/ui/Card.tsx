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
    <div className={clsx('flex flex-wrap items-start justify-between gap-3 border-b border-border px-5 py-4', className)}>
      <div className="flex min-w-0 items-start gap-3">
        {icon && <div className="mt-0.5 shrink-0 text-fg-muted">{icon}</div>}
        <div className="min-w-0">
          <h2 className="text-[15px] font-semibold leading-6 text-fg">{title}</h2>
          {description && <p className="mt-0.5 text-[13px] text-fg-muted">{description}</p>}
        </div>
      </div>
      {actions && <div className="flex shrink-0 items-center gap-2">{actions}</div>}
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
}: {
  label: React.ReactNode
  children: React.ReactNode
  hint?: React.ReactNode
}) {
  return (
    <div className="grid grid-cols-1 gap-1 px-5 py-3 sm:grid-cols-[minmax(160px,220px)_1fr] sm:gap-6">
      <dt className="text-[13px] text-fg-muted">
        {label}
        {hint && <div className="mt-0.5 text-xs text-fg-muted/80">{hint}</div>}
      </dt>
      <dd className="min-w-0 text-sm text-fg">{children}</dd>
    </div>
  )
}
