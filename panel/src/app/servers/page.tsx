'use client'

import { useEffect, useState } from 'react'
import useSWR from 'swr'
import {
  Plus,
  Trash2,
  Server as ServerIcon,
  CheckCircle2,
  XCircle,
  Radio,
} from 'lucide-react'
import { Layout } from '@/components/Layout'
import { Modal } from '@/components/Modal'
import { Badge } from '@/components/Badge'
import {
  FormField,
  inputClass,
  ModalActions,
  ErrorBox,
  ConfirmDelete,
  useStateReset,
} from '@/components/FormHelpers'
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
  const [newOpen, setNewOpen] = useState(false)
  const [deleting, setDeleting] = useState<ServerEntry | null>(null)
  const [activeUrl, setActiveUrl] = useState<string | null>(null)

  useEffect(() => {
    const s = getActiveServer()
    setActiveUrl(s?.url ?? null)
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

  const isActive = (s: ServerEntry) =>
    s.is_local ? activeUrl === null : s.url === activeUrl

  return (
    <div className="space-y-6 max-w-7xl">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-white">Servers</h1>
          <p className="text-sm text-gray-500 mt-1">
            Manage multiple origin servers from a single panel
          </p>
        </div>
        <button
          type="button"
          onClick={() => setNewOpen(true)}
          className="inline-flex items-center gap-1.5 px-3 py-1.5 text-sm rounded bg-blue-600 hover:bg-blue-500 text-white transition"
        >
          <Plus size={16} />
          Add Server
        </button>
      </div>

      <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-4">
        {servers.map((s) => (
          <ServerCard
            key={s.id}
            server={s}
            active={isActive(s)}
            localInfo={s.is_local ? localNode : undefined}
            onConnect={() => onConnect(s)}
            onDelete={() => setDeleting(s)}
          />
        ))}
      </div>

      <AddServerModal
        open={newOpen}
        onClose={() => setNewOpen(false)}
        onSaved={() => {
          setNewOpen(false)
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
          setDeleting(null)
          mutate()
        }}
      />
    </div>
  )
}

function ServerCard({
  server,
  active,
  localInfo,
  onConnect,
  onDelete,
}: {
  server: ServerEntry
  active: boolean
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

  const dot =
    status.state === 'ok'
      ? 'bg-green-500'
      : status.state === 'fail'
        ? 'bg-red-500'
        : 'bg-gray-500 animate-pulse'

  return (
    <div
      className={
        'rounded-lg border bg-gray-900 p-4 transition ' +
        (active ? 'border-blue-500 ring-1 ring-blue-500/30' : 'border-gray-800')
      }
    >
      <div className="flex items-start justify-between gap-2 mb-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <ServerIcon size={16} className="text-blue-400 flex-shrink-0" />
            <h3 className="text-white font-medium truncate">{server.name}</h3>
            {server.is_local && <Badge variant="blue">this server</Badge>}
            {active && !server.is_local && <Badge variant="green">active</Badge>}
          </div>
          <div className="text-xs text-gray-500 mt-1 truncate font-mono">
            {server.url || 'same origin'}
          </div>
        </div>
        <span className={'inline-block h-2.5 w-2.5 rounded-full ' + dot} />
      </div>

      {server.note && (
        <div className="text-xs text-gray-400 mb-3 line-clamp-2">{server.note}</div>
      )}

      <div className="grid grid-cols-2 gap-2 text-xs mb-4">
        <div>
          <div className="text-[10px] text-gray-500 uppercase tracking-wider">
            Status
          </div>
          <div className="flex items-center gap-1 mt-0.5">
            {status.state === 'ok' ? (
              <>
                <CheckCircle2 size={12} className="text-green-400" />
                <span className="text-green-400">Online</span>
              </>
            ) : status.state === 'fail' ? (
              <>
                <XCircle size={12} className="text-red-400" />
                <span className="text-red-400">Unreachable</span>
              </>
            ) : (
              <span className="text-gray-500">Checking…</span>
            )}
          </div>
        </div>
        <div>
          <div className="text-[10px] text-gray-500 uppercase tracking-wider">
            Streams
          </div>
          <div className="flex items-center gap-1 mt-0.5 text-gray-300">
            <Radio size={11} />
            {status.info?.stream_count ?? '—'}
          </div>
        </div>
        <div className="col-span-2">
          <div className="text-[10px] text-gray-500 uppercase tracking-wider">
            Version
          </div>
          <div className="text-gray-300 mt-0.5">
            {status.info?.version ?? '—'}
          </div>
        </div>
      </div>

      <div className="flex gap-2">
        <button
          type="button"
          onClick={onConnect}
          disabled={active}
          className="flex-1 px-2 py-1.5 text-xs rounded bg-blue-600 hover:bg-blue-500 disabled:opacity-50 disabled:cursor-not-allowed text-white transition"
        >
          {active ? 'Connected' : 'Connect'}
        </button>
        {!server.is_local && (
          <button
            type="button"
            onClick={onDelete}
            className="px-2 py-1.5 text-xs rounded bg-red-600/20 hover:bg-red-600/30 text-red-400 border border-red-600/40 transition"
            title="Remove from list"
          >
            <Trash2 size={12} />
          </button>
        )}
      </div>
    </div>
  )
}

function AddServerModal({
  open,
  onClose,
  onSaved,
}: {
  open: boolean
  onClose: () => void
  onSaved: () => void
}) {
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
    <Modal open={open} onClose={() => !busy && onClose()} title="Add server">
      <form onSubmit={onSubmit} className="space-y-3">
        <FormField label="Name">
          <input
            required
            value={form.name}
            onChange={(e) => setForm({ ...form, name: e.target.value })}
            className={inputClass}
            placeholder="EU origin 2"
          />
        </FormField>
        <FormField label="URL" hint="The HTTP base URL of the remote server">
          <input
            required
            type="url"
            value={form.url}
            onChange={(e) => setForm({ ...form, url: e.target.value })}
            className={inputClass}
            placeholder="http://server2.example.com:8080"
          />
        </FormField>
        <FormField label="Note">
          <textarea
            value={form.note}
            onChange={(e) => setForm({ ...form, note: e.target.value })}
            className={inputClass}
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
