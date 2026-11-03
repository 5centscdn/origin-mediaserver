'use client'

import React from 'react'
import clsx from 'clsx'

// Variant names are kept for existing call sites; they map onto the
// semantic palette: green = success, red = danger, yellow/orange = warning,
// blue = info, purple = primary accent, gray = neutral.
export type BadgeVariant = 'green' | 'red' | 'yellow' | 'blue' | 'gray' | 'orange' | 'purple'

interface BadgeProps {
  variant: BadgeVariant
  children: React.ReactNode
  /** Monospace text, for protocol / codec names. */
  mono?: boolean
  className?: string
  title?: string
}

const styles: Record<BadgeVariant, string> = {
  green: 'bg-success/10 text-success border-success/30',
  red: 'bg-danger/10 text-danger border-danger/30',
  yellow: 'bg-warning/10 text-warning border-warning/30',
  orange: 'bg-warning/10 text-warning border-warning/30',
  blue: 'bg-info/10 text-info border-info/30',
  purple: 'bg-primary/15 text-primary-fg border-primary/35',
  gray: 'bg-elevated text-fg-secondary border-border',
}

export function Badge({ variant, children, mono, className, title }: BadgeProps) {
  return (
    <span
      title={title}
      className={clsx(
        'inline-flex items-center gap-1 whitespace-nowrap rounded-md border px-1.5 py-0.5 text-xs font-medium leading-4',
        mono && 'font-mono',
        styles[variant],
        className,
      )}
    >
      {children}
    </span>
  )
}
