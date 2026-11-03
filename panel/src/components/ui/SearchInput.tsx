'use client'

import React from 'react'
import clsx from 'clsx'
import { Search, X } from 'lucide-react'

export function SearchInput({
  value,
  onChange,
  placeholder = 'Search…',
  className,
}: {
  value: string
  onChange: (v: string) => void
  placeholder?: string
  className?: string
}) {
  return (
    <div className={clsx('relative w-full sm:w-64', className)}>
      <Search size={14} className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-fg-muted" />
      <input
        type="search"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        aria-label={placeholder}
        className="h-8 w-full rounded-md border border-border bg-canvas/60 pl-8 pr-7 text-[13px] text-fg placeholder:text-fg-muted focus:border-primary focus:outline-none focus:ring-1 focus:ring-primary/40"
      />
      {value && (
        <button
          type="button"
          onClick={() => onChange('')}
          aria-label="Clear search"
          className="absolute right-1.5 top-1/2 -translate-y-1/2 rounded p-0.5 text-fg-muted hover:text-fg"
        >
          <X size={13} />
        </button>
      )}
    </div>
  )
}
