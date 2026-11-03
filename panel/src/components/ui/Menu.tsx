'use client'

import React, { useEffect, useLayoutEffect, useRef, useState } from 'react'
import clsx from 'clsx'
import { MoreHorizontal } from 'lucide-react'
import { iconButtonClass } from './Button'

export interface MenuItem {
  label: string
  icon?: React.ReactNode
  onClick?: () => void
  /** Renders a link instead (opened in a new tab when external). */
  href?: string
  external?: boolean
  danger?: boolean
  disabled?: boolean
  /** Draw a separator above this item. */
  separator?: boolean
}

/**
 * Overflow menu for less-used row actions. Positioned with `fixed` from the
 * trigger's rect, so it isn't clipped by a table's overflow container.
 */
export function DropdownMenu({
  items,
  label = 'More actions',
  trigger,
}: {
  items: MenuItem[]
  label?: string
  trigger?: React.ReactNode
}) {
  const [open, setOpen] = useState(false)
  const [pos, setPos] = useState<{ top: number; left: number; up: boolean }>({ top: 0, left: 0, up: false })
  const btn = useRef<HTMLButtonElement>(null)
  const menu = useRef<HTMLDivElement>(null)

  const place = () => {
    const r = btn.current?.getBoundingClientRect()
    if (!r) return
    const width = 208
    const up = r.bottom + 260 > window.innerHeight && r.top > 260
    setPos({
      top: up ? r.top - 4 : r.bottom + 4,
      left: Math.max(8, Math.min(r.right - width, window.innerWidth - width - 8)),
      up,
    })
  }

  useLayoutEffect(() => {
    if (open) place()
  }, [open])

  useEffect(() => {
    if (!open) return
    const close = (e: Event) => {
      if (e.type === 'mousedown' && (menu.current?.contains(e.target as Node) || btn.current?.contains(e.target as Node))) return
      setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setOpen(false)
        btn.current?.focus()
      }
    }
    document.addEventListener('mousedown', close)
    window.addEventListener('scroll', close, true)
    window.addEventListener('resize', close)
    document.addEventListener('keydown', onKey)
    menu.current?.querySelector<HTMLElement>('[role="menuitem"]:not([disabled])')?.focus()
    return () => {
      document.removeEventListener('mousedown', close)
      window.removeEventListener('scroll', close, true)
      window.removeEventListener('resize', close)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  const onMenuKey = (e: React.KeyboardEvent) => {
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return
    e.preventDefault()
    const els = Array.from(menu.current?.querySelectorAll<HTMLElement>('[role="menuitem"]:not([disabled])') ?? [])
    const i = els.indexOf(document.activeElement as HTMLElement)
    els[(i + (e.key === 'ArrowDown' ? 1 : -1) + els.length) % els.length]?.focus()
  }

  const itemClass = (it: MenuItem) =>
    clsx(
      'flex w-full items-center gap-2 rounded px-2.5 py-1.5 text-left text-[13px] transition-colors focus:outline-none',
      it.disabled
        ? 'cursor-not-allowed opacity-50'
        : it.danger
          ? 'text-danger hover:bg-danger/10 focus:bg-danger/10'
          : 'text-fg-secondary hover:bg-border/60 hover:text-fg focus:bg-border/60 focus:text-fg',
    )

  return (
    <>
      <button
        ref={btn}
        type="button"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={label}
        title={label}
        onClick={(e) => {
          e.stopPropagation()
          setOpen((o) => !o)
        }}
        className={trigger ? undefined : iconButtonClass('ghost', 'xs')}
      >
        {trigger ?? <MoreHorizontal size={15} />}
      </button>
      {open && (
        <div
          ref={menu}
          role="menu"
          onKeyDown={onMenuKey}
          style={{ top: pos.top, left: pos.left, transform: pos.up ? 'translateY(-100%)' : undefined }}
          className="fixed z-[55] w-52 rounded-lg border border-border bg-elevated p-1 shadow-pop"
        >
          {items.map((it, i) => (
            <React.Fragment key={i}>
              {it.separator && <div className="my-1 h-px bg-border" />}
              {it.href && !it.disabled ? (
                <a
                  role="menuitem"
                  href={it.href}
                  target={it.external ? '_blank' : undefined}
                  rel={it.external ? 'noreferrer' : undefined}
                  className={itemClass(it)}
                  onClick={() => setOpen(false)}
                >
                  {it.icon}
                  {it.label}
                </a>
              ) : (
                <button
                  role="menuitem"
                  type="button"
                  disabled={it.disabled}
                  className={itemClass(it)}
                  onClick={(e) => {
                    e.stopPropagation()
                    setOpen(false)
                    it.onClick?.()
                  }}
                >
                  {it.icon}
                  {it.label}
                </button>
              )}
            </React.Fragment>
          ))}
        </div>
      )}
    </>
  )
}
