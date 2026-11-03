'use client'

import { useEffect, useState } from 'react'
import useSWR from 'swr'
import clsx from 'clsx'
import { Plus, Trash2, Server as ServerIcon, Radio, Plug, Tag, Copy as CopyIcon } from 'lucide-react'
import { Layout } from '@/components/Layout'
import { Modal } from '@/components/Modal'
import { Badge } from '@/components/Badge'
import {
  FormField,
  inputClass,
  textareaClass,
  ModalActions,
  ErrorBox,
  ConfirmDelete,
  useStateReset,
} from '@/components/FormHelpers'
import { PageHeader, Button, IconButton, CopyButton, StatusBadge, Skeleton, useToast } from '@/components/ui'
import {
  listServers,
  addServer,
  deleteServer,
  pingServer,
  getActiveServer,
  setActiveServer,
  getNodeInfo,
  ServerEntry,
  NodeInfo,
} from '@/lib/api'

// normURL compares server URLs loosely (case, trailing slashes), the same
// way setActiveServer strips trailing slashes before using one.
function normURL(u: string): string {
  return u.trim().replace(/\/+$/, '').toLowerCase()
}

export default function ServersPage() {
  return (
    <Layout>
      <ServersContent />
    </Layout>
  )
}

function ServersContent() {
  const { data: registered, mutate } = useSWR('servers', listServers, {
    refreshInterval: 30000,
  })
  const { data: localNode } = useSWR('node', getNodeInfo, {
    refreshInterval: 10000,
  })
  const toast = useToast()
  const [newOpen, setNewOpen] = useState(false)
  const [deleting, setDeleting] = useState<ServerEntry | null>(null)
  const [activeUrl, setActiveUrl] = useState<string | null>(null)
  const [origin, setOrigin] = useState('')

  useEffect(() => {
    const s = getActiveServer()
    setActiveUrl(s?.url ?? null)
    setOrigin(window.location.origin)
  }, [])

  const localCard: ServerEntry = {
    id: '__local__',
    name: localNode?.node_name || 'This server',
    url: '',
    note: 'Running on this origin',
    created_at: '',
    is_local: true,
  }

  const servers: ServerEntry[] = [localCard, ...(registered ?? [])]

  const onConnect = (s: ServerEntry) => {
    if (s.is_local) {
      setActiveServer(null)
      setActiveUrl(null)
    } else {
      setActiveServer(s)
      setActiveUrl(s.url)
    }
    // Reload so SWR caches are dropped and all data is fetched from the new
    // server.
    window.location.reload()
  }

  const isActive = (s: ServerEntry) => (s.is_local ? activeUrl === null : s.url === activeUrl)

  // duplicateOf names the entry a saved server repeats: this server when the
  // URL is this panel's own origin, else the first earlier entry with the
  // same URL. Display only; records are left as they are.
  const duplicateOf = (s: ServerEntry, idx: number): string | null => {
    if (s.is_local || !s.url) return null
    const u = normURL(s.url)
    if (origin && u === normURL(origin)) return localCard.name
    const prev = servers.slice(1, idx).find((o) => o.url && normURL(o.url) === u)
    return prev ? prev.name : null
  }

  return (
    <>
      <PageHeader
        title="Servers"
        description="Manage multiple origin servers from a single panel. Connecting switches every page to that server."
        actions={
          <Button variant="primary" icon={<Plus size={15} />} onClick={() => setNewOpen(true)}>
            Add server
          </Button>
        }
      />

      <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
        {servers.map((s, i) => (
          <ServerCard
            key={s.id}
            server={s}
            origin={origin}
            active={isActive(s)}
            duplicateOf={duplicateOf(s, i)}
            localInfo={s.is_local ? localNode : undefined}
            onConnect={() => onConnect(s)}
            onDelete={() => setDeleting(s)}
          />
        ))}
        <button
          type="button"
          onClick={() => setNewOpen(true)}
          className="flex min-h-[200px] flex-col items-center justify-center gap-2 rounded-lg border border-dashed border-border-strong bg-transparent p-5 text-fg-muted transition-colors hover:border-primary/60 hover:bg-elevated/40 hover:text-fg focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary-fg"
        >
          <span className="flex h-10 w-10 items-center justify-center rounded-full border border-border bg-elevated">
            <Plus size={18} />
          </span>
          <span className="text-sm font-medium">Add server</span>
          <span className="text-xs">Register another origin by its HTTP URL</span>
        </button>
      </div>

      <AddServerModal
        open={newOpen}
        onClose={() => setNewOpen(false)}
        onSaved={() => {
          setNewOpen(false)
          toast.success('Server added')
          mutate()
        }}
      />

      <ConfirmDelete
        open={!!deleting}
        title="Remove server"
        message={`Remove "${deleting?.name ?? ''}" from the server list? This won't affect the remote server itself.`}
        confirmLabel="Remove"
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting) return
          await deleteServer(deleting.id)
          // If we removed the active server, fall back to local.
          if (deleting.url && deleting.url === activeUrl) {
            setActiveServer(null)
            setActiveUrl(null)
          }
          toast.success(`Removed ${deleting.name}`)
          setDeleting(null)
          mutate()
        }}
      />
    </>
  )
}

function ServerCard({
  server,
  origin,
  active,
  duplicateOf,
  localInfo,
  onConnect,
  onDelete,
}: {
  server: ServerEntry
  origin: string
  active: boolean
  duplicateOf: string | null
  localInfo?: NodeInfo
  onConnect: () => void
  onDelete: () => void
}) {
  const [status, setStatus] = useState<{
    state: 'pending' | 'ok' | 'fail'
    info?: NodeInfo
  }>({ state: 'pending' })

  useEffect(() => {
    let cancelled = false
    if (server.is_local) {
      if (localInfo) {
        setStatus({ state: 'ok', info: localInfo })
      } else {
        setStatus({ state: 'fail' })
      }
      return
    }
    setStatus({ state: 'pending' })
    pingServer(server.url).then((r) => {
      if (cancelled) return
      setStatus(r.ok ? { state: 'ok', info: r.info } : { state: 'fail' })
    })
    return () => {
      cancelled = true
    }
  }, [server.id, server.url, server.is_local, localInfo])

  const shownURL = server.url || origin

  return (
    <div
      className={clsx(
        'flex flex-col rounded-lg border bg-surface transition-colors',
        active ? 'border-primary ring-1 ring-primary/30' : 'border-border',
      )}
    >
      <div className="flex items-start justify-between gap-3 border-b border-border px-5 py-4">
        <div className="flex min-w-0 items-start gap-3">
          <div
            className={clsx(
              'flex h-9 w-9 shrink-0 items-center justify-center rounded-md border',
              active ? 'border-primary/40 bg-primary/15 text-primary-fg' : 'border-border bg-elevated text-fg-muted',
            )}
          >
            <ServerIcon size={17} />
          </div>
          <div className="min-w-0">
            <h3 className="truncate text-[15px] font-semibold leading-6 text-fg" title={server.name}>
              {server.name}
            </h3>
            <div className="mt-1 flex flex-wrap items-center gap-1.5">
              {server.is_local && <Badge variant="blue">This server</Badge>}
              {active && <StatusBadge status="success" label="Connected" />}
              {duplicateOf && (
                <Badge variant="yellow" title="Another entry points to the same URL">
                  <CopyIcon size={11} />
                  Duplicate of {duplicateOf}
                </Badge>
              )}
            </div>
          </div>
        </div>
        {status.state === 'ok' ? (
          <StatusBadge status="online" />
        ) : status.state === 'fail' ? (
          <StatusBadge status="offline" label="Unreachable" />
        ) : (
          <StatusBadge status="idle" label="Checking…" />
        )}
      </div>

      <div className="flex-1 space-y-4 px-5 py-4">
        <div className="flex min-w-0 items-center gap-1">
          <code
            className="min-w-0 flex-1 truncate font-mono text-xs text-fg-secondary"
            title={shownURL || 'same origin'}
          >
            {shownURL || 'same origin'}
          </code>
          {shownURL && <CopyButton text={shownURL} title="Copy server URL" toastMessage="Server URL copied" />}
        </div>

        {server.note && <p className="line-clamp-2 text-[13px] text-fg-muted">{server.note}</p>}

        <dl className="grid grid-cols-2 gap-3">
          <div className="rounded-md border border-border bg-canvas/40 px-3 py-2">
            <dt className="flex items-center gap-1.5 text-xs text-fg-muted">
              <Radio size={12} />
              Streams
            </dt>
            <dd className="tabular mt-0.5 text-lg font-semibold text-fg">
              {status.state === 'pending' ? <Skeleton className="mt-1 h-5 w-8" /> : status.info?.stream_count ?? '—'}
            </dd>
          </div>
          <div className="min-w-0 rounded-md border border-border bg-canvas/40 px-3 py-2">
            <dt className="flex items-center gap-1.5 text-xs text-fg-muted">
              <Tag size={12} />
              Version
            </dt>
            <dd className="mt-1 truncate font-mono text-[13px] text-fg" title={status.info?.version}>
              {status.state === 'pending' ? <Skeleton className="mt-1 h-4 w-16" /> : status.info?.version ?? '—'}
            </dd>
          </div>
        </dl>
      </div>

      <div className="flex items-center gap-2 border-t border-border px-5 py-3">
        <Button
          variant={active ? 'secondary' : 'primary'}
          size="sm"
          className="flex-1"
          icon={<Plug size={14} />}
          onClick={onConnect}
          disabled={active}
        >
          {active ? 'Connected' : 'Connect'}
        </Button>
        {!server.is_local && (
          <IconButton label={`Remove ${server.name} from list`} variant="danger-outline" size="sm" onClick={onDelete}>
            <Trash2 size={14} />
          </IconButton>
        )}
      </div>
    </div>
  )
}

function AddServerModal({ open, onClose, onSaved }: { open: boolean; onClose: () => void; onSaved: () => void }) {
  const [form, setForm] = useState({ name: '', url: '', note: '' })
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  useStateReset(JSON.stringify(open), () => {
    if (open) {
      setForm({ name: '', url: '', note: '' })
      setErr(null)
    }
  })

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr(null)
    try {
      await addServer(form)
      onSaved()
    } catch (e: any) {
      setErr(e?.message ?? 'Save failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={() => !busy && onClose()}
      title="Add server"
      description="Register another origin so you can switch to it from this panel."
    >
      <form onSubmit={onSubmit} className="space-y-4">
        <FormField label="Name" required>
          <input
            required
            value={form.name}
            onChange={(e) => setForm({ ...form, name: e.target.value })}
            className={inputClass}
            placeholder="EU origin 2"
          />
        </FormField>
        <FormField label="URL" required hint="The HTTP base URL of the remote server">
          <input
            required
            type="url"
            value={form.url}
            onChange={(e) => setForm({ ...form, url: e.target.value })}
            className={inputClass + ' font-mono text-[13px]'}
            placeholder="http://server2.example.com:8080"
          />
        </FormField>
        <FormField label="Note">
          <textarea
            value={form.note}
            onChange={(e) => setForm({ ...form, note: e.target.value })}
            className={textareaClass.replace(' font-mono text-xs', ' text-sm')}
            rows={2}
            placeholder="Optional description"
          />
        </FormField>
        <ErrorBox msg={err} />
        <ModalActions busy={busy} onCancel={onClose} submitLabel="Add" />
      </form>
    </Modal>
  )
}
