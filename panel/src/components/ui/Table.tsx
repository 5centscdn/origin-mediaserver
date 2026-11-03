'use client'

import React from 'react'
import clsx from 'clsx'
import { ArrowDown, ArrowUp, ArrowUpDown } from 'lucide-react'

/**
 * Data table building blocks. Usage:
 *
 *   <TableCard>
 *     <Table>
 *       <THead><tr><TH>Name</TH><TH align="right">Viewers</TH><TH align="right">Actions</TH></tr></THead>
 *       <TBody>
 *         <TR><TD>…</TD><TD align="right" numeric>…</TD><TD align="right"><RowActions>…</RowActions></TD></TR>
 *       </TBody>
 *     </Table>
 *   </TableCard>
 *
 * TableCard scrolls horizontally on narrow screens instead of overflowing
 * the page.
 */
export function TableCard({
  children,
  className,
  header,
  footer,
}: {
  children: React.ReactNode
  className?: string
  /** Optional toolbar above the table (search, filters). */
  header?: React.ReactNode
  footer?: React.ReactNode
}) {
  return (
    <div className={clsx('relative overflow-hidden rounded-lg border border-border bg-surface', className)}>
      {header && <div className="flex flex-wrap items-center gap-3 border-b border-border px-4 py-3">{header}</div>}
      <div className="relative overflow-x-auto">{children}</div>
      {footer && <div className="border-t border-border px-4 py-2.5 text-xs text-fg-muted">{footer}</div>}
    </div>
  )
}

export function Table({ children, className }: { children: React.ReactNode; className?: string }) {
  return <table className={clsx('w-full border-collapse text-[13px]', className)}>{children}</table>
}

export function THead({ children }: { children: React.ReactNode }) {
  return <thead className="border-b border-border bg-elevated/40">{children}</thead>
}

type Align = 'left' | 'right' | 'center'
const alignClass: Record<Align, string> = { left: 'text-left', right: 'text-right', center: 'text-center' }

export function TH({
  children,
  align = 'left',
  className,
  sort,
  onSort,
}: {
  children?: React.ReactNode
  align?: Align
  className?: string
  /** For sortable columns: current state of this column. */
  sort?: 'asc' | 'desc' | false
  onSort?: () => void
}) {
  const label = onSort ? (
    <button
      type="button"
      onClick={onSort}
      className={clsx(
        // uppercase/tracking repeated: preflight resets text-transform on buttons.
        'inline-flex items-center gap-1 rounded uppercase tracking-wide transition-colors hover:text-fg',
        sort ? 'text-fg' : '',
        align === 'right' && 'flex-row-reverse',
      )}
    >
      {children}
      {sort === 'asc' ? (
        <ArrowUp size={12} />
      ) : sort === 'desc' ? (
        <ArrowDown size={12} />
      ) : (
        <ArrowUpDown size={12} className="opacity-40" />
      )}
    </button>
  ) : (
    children
  )
  return (
    <th
      scope="col"
      aria-sort={sort === 'asc' ? 'ascending' : sort === 'desc' ? 'descending' : undefined}
      className={clsx(
        'whitespace-nowrap px-4 py-2.5 text-xs font-medium uppercase tracking-wide text-fg-muted',
        alignClass[align],
        className,
      )}
    >
      {label}
    </th>
  )
}

export function TBody({ children }: { children: React.ReactNode }) {
  return <tbody className="divide-y divide-border">{children}</tbody>
}

export function TR({
  children,
  className,
  selected,
  onClick,
}: {
  children: React.ReactNode
  className?: string
  selected?: boolean
  onClick?: () => void
}) {
  return (
    <tr
      onClick={onClick}
      className={clsx(
        'transition-colors hover:bg-elevated/50',
        selected && 'bg-primary/10 hover:bg-primary/15',
        onClick && 'cursor-pointer',
        className,
      )}
    >
      {children}
    </tr>
  )
}

export function TD({
  children,
  align = 'left',
  numeric,
  mono,
  muted,
  className,
  colSpan,
}: {
  children?: React.ReactNode
  align?: Align
  /** Tabular figures, right-aligned by default. */
  numeric?: boolean
  mono?: boolean
  muted?: boolean
  className?: string
  colSpan?: number
}) {
  return (
    <td
      colSpan={colSpan}
      className={clsx(
        'px-4 py-3 align-middle',
        alignClass[numeric && align === 'left' ? 'right' : align],
        numeric && 'tabular whitespace-nowrap',
        mono && 'font-mono text-xs',
        muted ? 'text-fg-muted' : 'text-fg-secondary',
        className,
      )}
    >
      {children}
    </td>
  )
}

/** Right-aligned row of action buttons for the last column. */
export function RowActions({ children, className }: { children: React.ReactNode; className?: string }) {
  return <div className={clsx('flex items-center justify-end gap-1', className)}>{children}</div>
}

/** Long text (URLs, IDs) cut to one line, full value in the tooltip. */
export function Truncate({
  children,
  title,
  className,
  mono,
  max = 'max-w-[240px]',
}: {
  children: React.ReactNode
  title?: string
  className?: string
  mono?: boolean
  /** Tailwind max-width class. */
  max?: string
}) {
  return (
    <span
      title={title ?? (typeof children === 'string' ? children : undefined)}
      className={clsx('block truncate', max, mono && 'font-mono text-xs', className)}
    >
      {children}
    </span>
  )
}
