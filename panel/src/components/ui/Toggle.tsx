'use client'

import React from 'react'
import clsx from 'clsx'

/** On/off switch (role="switch"), with optional label and description. */
export function Toggle({
  checked,
  onChange,
  label,
  description,
  disabled,
  id,
}: {
  checked: boolean
  onChange: (v: boolean) => void
  label?: React.ReactNode
  description?: React.ReactNode
  disabled?: boolean
  id?: string
}) {
  const sw = (
    <button
      id={id}
      type="button"
      role="switch"
      aria-checked={checked}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={clsx(
        'relative inline-flex h-5 w-9 shrink-0 items-center rounded-full border transition-colors disabled:cursor-not-allowed disabled:opacity-50',
        checked ? 'border-primary bg-primary' : 'border-border-strong bg-elevated',
      )}
    >
      <span
        className={clsx(
          'inline-block h-3.5 w-3.5 rounded-full bg-white shadow transition-transform',
          checked ? 'translate-x-[18px]' : 'translate-x-[2px]',
        )}
      />
    </button>
  )
  if (!label) return sw
  return (
    <label className={clsx('flex items-start gap-3', disabled ? 'cursor-not-allowed' : 'cursor-pointer')}>
      <span className="mt-0.5">{sw}</span>
      <span className="min-w-0">
        <span className="block text-sm text-fg">{label}</span>
        {description && <span className="mt-0.5 block text-xs text-fg-muted">{description}</span>}
      </span>
    </label>
  )
}
