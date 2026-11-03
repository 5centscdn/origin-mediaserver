'use client'

import React, { useEffect, useRef, useState } from 'react'
import { AlertTriangle, Loader2 } from 'lucide-react'
import { Modal } from './Modal'

export function FormField({
  label,
  children,
  hint,
}: {
  label: string
  children: React.ReactNode
  hint?: string
}) {
  return (
    <div>
      <label className="block text-xs uppercase tracking-wider text-gray-500 mb-1">
        {label}
      </label>
      {children}
      {hint && <div className="text-xs text-gray-500 mt-1">{hint}</div>}
    </div>
  )
}

export const inputClass =
  'w-full bg-gray-800 border border-gray-700 rounded-md px-3 py-2 text-sm text-white focus:outline-none focus:border-blue-500'

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
    <div className="sticky bottom-0 -mx-5 -mb-5 flex justify-end gap-2 border-t border-gray-800 bg-gray-900 px-5 pb-5 pt-3">
      <button
        type="button"
        onClick={onCancel}
        disabled={busy}
        className="px-3 py-1.5 text-sm rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
      >
        Cancel
      </button>
      <button
        type="submit"
        disabled={busy}
        className="px-3 py-1.5 text-sm rounded bg-blue-600 hover:bg-blue-500 disabled:opacity-50 text-white flex items-center gap-2 transition"
      >
        {busy && <Loader2 size={14} className="animate-spin" />}
        {submitLabel}
      </button>
    </div>
  )
}

export function ErrorBox({ msg }: { msg: string | null }) {
  if (!msg) return null
  return (
    <div className="text-sm text-red-400 bg-red-500/10 border border-red-500/30 rounded px-3 py-2">
      {msg}
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
        <div className="flex gap-2 text-sm text-gray-300">
          <AlertTriangle className="text-yellow-400 flex-shrink-0" size={18} />
          <div>{message}</div>
        </div>
        <ErrorBox msg={err} />
        <div className="flex justify-end gap-2 pt-2">
          <button
            type="button"
            onClick={onCancel}
            disabled={busy}
            className="px-3 py-1.5 text-sm rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
          >
            Cancel
          </button>
          <button
            type="button"
            onClick={run}
            disabled={busy}
            className="px-3 py-1.5 text-sm rounded bg-red-600 hover:bg-red-500 disabled:opacity-50 text-white flex items-center gap-2 transition"
          >
            {busy && <Loader2 size={14} className="animate-spin" />}
            {confirmLabel}
          </button>
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
