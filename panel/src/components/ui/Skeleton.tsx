'use client'

import React from 'react'
import clsx from 'clsx'

export function Skeleton({ className }: { className?: string }) {
  return <div className={clsx('animate-pulse rounded bg-elevated', className)} />
}

/** Placeholder rows while a table's first load is in flight. */
export function TableSkeleton({ rows = 4, cols = 5 }: { rows?: number; cols?: number }) {
  return (
    <div className="overflow-hidden rounded-lg border border-border bg-surface" aria-busy="true" aria-label="Loading">
      <div className="flex gap-6 border-b border-border bg-elevated/40 px-4 py-3">
        {Array.from({ length: cols }).map((_, i) => (
          <Skeleton key={i} className="h-3 flex-1" />
        ))}
      </div>
      {Array.from({ length: rows }).map((_, r) => (
        <div key={r} className="flex gap-6 border-b border-border px-4 py-4 last:border-0">
          {Array.from({ length: cols }).map((_, i) => (
            <Skeleton key={i} className={clsx('h-3.5 flex-1', i === 0 && 'max-w-[160px]')} />
          ))}
        </div>
      ))}
    </div>
  )
}

export function CardSkeleton({ className }: { className?: string }) {
  return (
    <div className={clsx('rounded-lg border border-border bg-surface p-5', className)}>
      <Skeleton className="h-3 w-24" />
      <Skeleton className="mt-3 h-7 w-16" />
    </div>
  )
}
