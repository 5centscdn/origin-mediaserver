'use client'

import React, { useEffect, useRef, useState } from 'react'

/**
 * Hover/focus tooltip. Wraps its trigger in an inline span and shows the
 * content after a short delay on hover, immediately on keyboard focus.
 * Positioned `fixed` from the trigger's rect, so a table's scroll container
 * can't clip it.
 *
 * It's visual only (aria-hidden): the trigger must carry its own accessible
 * name (aria-label), which IconButton and the sidebar links do, so screen
 * readers don't hear the text twice.
 */
export function Tooltip({
  content,
  children,
  side = 'top',
  disabled,
}: {
  content: React.ReactNode
  children: React.ReactNode
  side?: 'top' | 'right' | 'bottom'
  disabled?: boolean
}) {
  const ref = useRef<HTMLSpanElement>(null)
  const timer = useRef<ReturnType<typeof setTimeout>>()
  const [pos, setPos] = useState<{ x: number; y: number } | null>(null)

  const show = (delay: number) => {
    clearTimeout(timer.current)
    timer.current = setTimeout(() => {
      const r = ref.current?.getBoundingClientRect()
      if (!r) return
      if (side === 'right') setPos({ x: r.right + 8, y: r.top + r.height / 2 })
      else if (side === 'bottom') setPos({ x: r.left + r.width / 2, y: r.bottom + 6 })
      else setPos({ x: r.left + r.width / 2, y: r.top - 6 })
    }, delay)
  }
  const hide = () => {
    clearTimeout(timer.current)
    setPos(null)
  }

  useEffect(() => {
    if (!pos) return
    const close = () => hide()
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && close()
    window.addEventListener('scroll', close, true)
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('scroll', close, true)
      window.removeEventListener('keydown', onKey)
    }
  }, [pos])
  useEffect(() => () => clearTimeout(timer.current), [])

  if (disabled || !content) return <>{children}</>

  const transform =
    side === 'right' ? 'translateY(-50%)' : side === 'bottom' ? 'translateX(-50%)' : 'translate(-50%, -100%)'

  return (
    <span
      ref={ref}
      className="inline-flex"
      onMouseEnter={() => show(350)}
      onMouseLeave={hide}
      onFocus={(e) => {
        // Keyboard focus only; a mouse click focusing the button shouldn't pop it.
        if ((e.target as HTMLElement).matches(':focus-visible')) show(0)
      }}
      onBlur={hide}
      onMouseDown={hide}
    >
      {children}
      {pos && (
        <span
          aria-hidden
          style={{ left: pos.x, top: pos.y, transform }}
          className="pointer-events-none fixed z-[70] max-w-[260px] whitespace-normal rounded-md border border-border-strong bg-elevated px-2.5 py-1.5 text-xs font-medium leading-snug text-fg shadow-pop"
        >
          {content}
        </span>
      )}
    </span>
  )
}
