'use client'

import React, { useEffect, useRef, useState } from 'react'
import { AlertTriangle } from 'lucide-react'
import { Modal } from './Modal'
import { Button } from './ui/Button'

export function FormField({
  label,
  children,
  hint,
  error,
  required,
}: {
  label: string
  children: React.ReactNode
  hint?: React.ReactNode
  /** Validation message; shown in place of the hint. */
  error?: string | null
  required?: boolean
}) {
  return (
    <div>
      <label className="mb-1.5 block text-[13px] font-medium text-fg-secondary">
        {label}
        {required && <span className="ml-0.5 text-danger">*</span>}
      </label>
      {children}
      {error ? (
        <div className="mt-1.5 text-xs text-danger">{error}</div>
      ) : (
        hint && <div className="mt-1.5 text-xs leading-relaxed text-fg-muted">{hint}</div>
      )}
    </div>
  )
}

// Shared field styles. inputClass works for <input>, <select> and <textarea>.
export const inputClass =
  'w-full h-9 rounded-md border border-border bg-canvas/60 px-3 text-sm text-fg placeholder:text-fg-muted ' +
  'transition-colors hover:border-border-strong focus:border-primary focus:outline-none focus:ring-1 focus:ring-primary/40 ' +
  'disabled:cursor-not-allowed disabled:opacity-60'

export const textareaClass = inputClass.replace('h-9', 'min-h-[80px] py-2') + ' font-mono text-xs leading-relaxed'

export const checkboxClass =
  'h-4 w-4 rounded border-border-strong bg-canvas text-primary accent-[rgb(var(--primary))] focus:ring-primary/40'

/** Checkbox with label and optional description. */
export function CheckboxField({
  checked,
  onChange,
  label,
  description,
  disabled,
}: {
  checked: boolean
  onChange: (v: boolean) => void
  label: React.ReactNode
  description?: React.ReactNode
  disabled?: boolean
}) {
  return (
    <label className={'flex items-start gap-2.5 ' + (disabled ? 'cursor-not-allowed opacity-60' : 'cursor-pointer')}>
      <input
        type="checkbox"
        className={checkboxClass + ' mt-0.5'}
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
      />
      <span className="min-w-0">
        <span className="block text-sm text-fg">{label}</span>
        {description && <span className="mt-0.5 block text-xs text-fg-muted">{description}</span>}
      </span>
    </label>
  )
}

/** Groups related fields under a small heading inside a form. */
export function FormSection({
  title,
  description,
  children,
}: {
  title: string
  description?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <fieldset className="space-y-3 border-t border-border pt-4 first:border-0 first:pt-0">
      <legend className="sr-only">{title}</legend>
      <div>
        <div className="text-[13px] font-semibold text-fg">{title}</div>
        {description && <div className="mt-0.5 text-xs text-fg-muted">{description}</div>}
      </div>
      {children}
    </fieldset>
  )
}

export function ModalActions({
  busy,
  onCancel,
  submitLabel,
}: {
  busy: boolean
  onCancel: () => void
  submitLabel: string
}) {
  return (
    // Sticky inside Modal's scrolling body, so Save stays reachable on long
    // forms; the negative margins let its background cover the body padding.
    <div className="sticky bottom-0 -mx-5 -mb-5 mt-2 flex justify-end gap-2 border-t border-border bg-surface px-5 pb-5 pt-4">
      <Button variant="secondary" size="md" onClick={onCancel} disabled={busy}>
        Cancel
      </Button>
      <Button variant="primary" size="md" type="submit" loading={busy}>
        {submitLabel}
      </Button>
    </div>
  )
}

export function ErrorBox({ msg }: { msg: string | null }) {
  if (!msg) return null
  return (
    <div role="alert" className="flex items-start gap-2 rounded-md border border-danger/30 bg-danger/10 px-3 py-2 text-[13px] text-danger">
      <AlertTriangle size={15} className="mt-0.5 shrink-0" />
      <span className="min-w-0 break-words">{msg}</span>
    </div>
  )
}

export function ConfirmDelete({
  open,
  title,
  message,
  onCancel,
  onConfirm,
  confirmLabel = 'Delete',
}: {
  open: boolean
  title: string
  message: string
  onCancel: () => void
  onConfirm: () => Promise<void> | void
  confirmLabel?: string
}) {
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const initial = JSON.stringify(open)
  useStateReset(initial, () => {
    if (open) setErr(null)
  })
  const run = async () => {
    setBusy(true)
    setErr(null)
    try {
      await onConfirm()
    } catch (e: any) {
      setErr(e?.message ?? 'Failed')
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal open={open} onClose={() => !busy && onCancel()} title={title}>
      <div className="space-y-3">
        <div className="flex gap-3 text-sm text-fg-secondary">
          <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-danger/10 text-danger">
            <AlertTriangle size={16} />
          </div>
          <div className="pt-1 leading-relaxed">{message}</div>
        </div>
        <ErrorBox msg={err} />
        <div className="flex justify-end gap-2 pt-2">
          <Button variant="secondary" size="md" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button variant="destructive" size="md" onClick={run} loading={busy}>
            {confirmLabel}
          </Button>
        </div>
      </div>
    </Modal>
  )
}

// Run a side-effect whenever `key` changes. Useful for re-initialising
// form state when a modal opens or the edit target changes.
export function useStateReset(key: string, fn: () => void) {
  const prev = useRef<string | null>(null)
  useEffect(() => {
    if (prev.current !== key) {
      prev.current = key
      fn()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key])
}
