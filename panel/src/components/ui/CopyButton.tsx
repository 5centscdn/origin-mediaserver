'use client'

import React, { useState } from 'react'
import clsx from 'clsx'
import { Check, Copy } from 'lucide-react'
import { copyToClipboard } from '@/lib/utils'
import { buttonClass, iconButtonClass, ButtonSize, ButtonVariant } from './Button'
import { useToast } from './Toast'
import { Tooltip } from './Tooltip'

/**
 * Copies `text`, shows a check for 1.5 s and a toast. With `label` it's a
 * labelled button ("HLS"); without, an icon button whose tooltip is
 * `title` (default "Copy").
 */
export function CopyButton({
  text,
  label,
  title,
  size = 'xs',
  variant = 'ghost',
  className,
  toastMessage,
}: {
  text: string
  label?: React.ReactNode
  title?: string
  size?: ButtonSize
  variant?: ButtonVariant
  className?: string
  toastMessage?: string
}) {
  const [done, setDone] = useState(false)
  const toast = useToast()
  const onClick = async (e: React.MouseEvent) => {
    e.stopPropagation()
    if (await copyToClipboard(text)) {
      setDone(true)
      toast.success(toastMessage ?? 'Copied to clipboard')
      setTimeout(() => setDone(false), 1500)
    } else {
      toast.error('Copy failed: select the text and copy it manually')
    }
  }
  const icon = done ? <Check size={13} className="text-success" /> : <Copy size={13} />
  const tip = title ?? (typeof label === 'string' ? `Copy ${label} URL` : 'Copy')
  if (!label) {
    return (
      <Tooltip content={done ? 'Copied' : tip}>
        <button type="button" onClick={onClick} aria-label={tip} className={iconButtonClass(variant, size, className)}>
          {icon}
        </button>
      </Tooltip>
    )
  }
  return (
    <Tooltip content={tip}>
      <button type="button" onClick={onClick} aria-label={tip} className={buttonClass(variant, size, clsx(className))}>
        {icon}
        {label}
      </button>
    </Tooltip>
  )
}

/** A URL/value shown in monospace with a copy button next to it. */
export function CopyField({ value, className }: { value: string; className?: string }) {
  return (
    <div className={clsx('flex min-w-0 items-center gap-1 rounded-md border border-border bg-canvas/60 pl-3 pr-1', className)}>
      <code className="min-w-0 flex-1 truncate py-1.5 font-mono text-xs text-fg-secondary" title={value}>
        {value}
      </code>
      <CopyButton text={value} />
    </div>
  )
}
