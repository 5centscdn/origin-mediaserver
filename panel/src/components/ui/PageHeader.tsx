'use client'

import React from 'react'

/** Page title row: title + description on the left, actions on the right. */
export function PageHeader({
  title,
  description,
  actions,
  meta,
}: {
  title: React.ReactNode
  description?: React.ReactNode
  actions?: React.ReactNode
  /** Small status text under the actions (e.g. "Auto-refresh 3s"). */
  meta?: React.ReactNode
}) {
  return (
    <div className="flex flex-wrap items-end justify-between gap-4">
      <div className="min-w-0">
        <h1 className="text-2xl font-semibold tracking-tight text-fg">{title}</h1>
        {description && <p className="mt-1 max-w-3xl text-sm text-fg-muted">{description}</p>}
      </div>
      {(actions || meta) && (
        <div className="flex flex-wrap items-center gap-3">
          {meta && <span className="text-xs text-fg-muted">{meta}</span>}
          {actions}
        </div>
      )}
    </div>
  )
}

/** Section heading inside a page (between cards). */
export function SectionTitle({
  children,
  actions,
  description,
}: {
  children: React.ReactNode
  actions?: React.ReactNode
  description?: React.ReactNode
}) {
  return (
    <div className="flex flex-wrap items-end justify-between gap-3">
      <div>
        <h2 className="text-base font-semibold text-fg">{children}</h2>
        {description && <p className="mt-0.5 text-[13px] text-fg-muted">{description}</p>}
      </div>
      {actions && <div className="flex items-center gap-2">{actions}</div>}
    </div>
  )
}
