'use client'

import React from 'react'
import clsx from 'clsx'
import { Loader2 } from 'lucide-react'

export type ButtonVariant = 'primary' | 'secondary' | 'outline' | 'ghost' | 'destructive' | 'danger-outline'
export type ButtonSize = 'xs' | 'sm' | 'md'

const base =
  'inline-flex items-center justify-center gap-1.5 whitespace-nowrap font-medium rounded-md transition-colors ' +
  'disabled:opacity-50 disabled:cursor-not-allowed disabled:pointer-events-none ' +
  'focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary-fg'

const variants: Record<ButtonVariant, string> = {
  primary: 'bg-primary text-white hover:bg-primary-hover shadow-sm',
  secondary: 'bg-elevated text-fg border border-border hover:bg-border hover:border-border-strong',
  outline: 'border border-border-strong text-fg-secondary hover:text-fg hover:bg-elevated',
  ghost: 'text-fg-secondary hover:text-fg hover:bg-elevated',
  destructive: 'bg-danger-strong text-white hover:bg-danger shadow-sm',
  'danger-outline': 'border border-danger/40 text-danger hover:bg-danger/10 hover:border-danger/60',
}

const sizes: Record<ButtonSize, string> = {
  xs: 'h-7 px-2 text-xs',
  sm: 'h-8 px-3 text-[13px]',
  md: 'h-9 px-4 text-sm',
}

const iconSizes: Record<ButtonSize, string> = {
  xs: 'h-7 w-7',
  sm: 'h-8 w-8',
  md: 'h-9 w-9',
}

/** Class string for a button look, for <a> links that should look like buttons. */
export function buttonClass(variant: ButtonVariant = 'secondary', size: ButtonSize = 'sm', extra?: string) {
  return clsx(base, variants[variant], sizes[size], extra)
}

/** Class string for a square icon-only button. */
export function iconButtonClass(variant: ButtonVariant = 'ghost', size: ButtonSize = 'sm', extra?: string) {
  return clsx(base, variants[variant], iconSizes[size], extra)
}

export interface ButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant
  size?: ButtonSize
  loading?: boolean
  icon?: React.ReactNode
}

export const Button = React.forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = 'secondary', size = 'sm', loading, icon, className, children, disabled, type = 'button', ...rest },
  ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      disabled={disabled || loading}
      className={buttonClass(variant, size, className)}
      {...rest}
    >
      {loading ? <Loader2 size={14} className="animate-spin" /> : icon}
      {children}
    </button>
  )
})

export interface IconButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  /** Required: shown as the tooltip and read by screen readers. */
  label: string
  variant?: ButtonVariant
  size?: ButtonSize
  loading?: boolean
}

/** Icon-only button. `label` becomes the native tooltip and aria-label. */
export const IconButton = React.forwardRef<HTMLButtonElement, IconButtonProps>(function IconButton(
  { label, variant = 'ghost', size = 'sm', loading, className, children, disabled, type = 'button', ...rest },
  ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      title={label}
      aria-label={label}
      disabled={disabled || loading}
      className={iconButtonClass(variant, size, className)}
      {...rest}
    >
      {loading ? <Loader2 size={14} className="animate-spin" /> : children}
    </button>
  )
})
