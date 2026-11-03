'use client'

import React, { useEffect, useRef } from 'react'
import { X } from 'lucide-react'
import clsx from 'clsx'

interface ModalProps {
  open: boolean
  onClose: () => void
  title: string
  children: React.ReactNode
  size?: 'sm' | 'md' | 'lg' | 'xl'
  /** Optional line under the title. */
  description?: React.ReactNode
}

const sizes = {
  sm: 'max-w-sm',
  md: 'max-w-md',
  lg: 'max-w-2xl',
  xl: 'max-w-5xl',
}

export function Modal({ open, onClose, title, children, size = 'md', description }: ModalProps) {
  const panel = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    // Move focus into the dialog (first field, else the dialog itself) and
    // give it back to whatever opened it on close.
    const prev = document.activeElement as HTMLElement | null
    const first = panel.current?.querySelector<HTMLElement>(
      'input:not([type=hidden]):not([disabled]), select:not([disabled]), textarea:not([disabled])',
    )
    ;(first ?? panel.current)?.focus()
    const overflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = overflow
      prev?.focus?.()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  if (!open) return null

  return (
    <div
      // !mt-0: modals render inside pages whose space-y-* would push a fixed
      // overlay down by the gap.
      className="fixed inset-0 z-50 !mt-0 flex items-end justify-center bg-black/60 p-0 backdrop-blur-[2px] sm:items-center sm:p-4"
      onMouseDown={onClose}
    >
      <div
        ref={panel}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        tabIndex={-1}
        // Capped to the viewport: a long form (e.g. Edit Application)
        // scrolls inside the body instead of pushing its Save button off
        // screen. ModalActions stays pinned to the bottom while it scrolls.
        className={clsx(
          'flex max-h-[92vh] w-full flex-col rounded-t-xl border border-border bg-surface shadow-pop focus:outline-none sm:max-h-full sm:rounded-xl',
          sizes[size],
        )}
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="flex shrink-0 items-start justify-between gap-4 border-b border-border px-5 py-4">
          <div className="min-w-0">
            <h2 className="text-base font-semibold text-fg">{title}</h2>
            {description && <p className="mt-0.5 text-[13px] text-fg-muted">{description}</p>}
          </div>
          <button
            type="button"
            onClick={onClose}
            className="-mr-1 rounded-md p-1 text-fg-muted transition-colors hover:bg-elevated hover:text-fg"
            aria-label="Close"
          >
            <X size={18} />
          </button>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto overscroll-contain p-5">{children}</div>
      </div>
    </div>
  )
}
