'use client'

import { useMemo, useState } from 'react'
import useSWR from 'swr'
import { Plus, Server, Network, Radio, Activity, ExternalLink, Trash2, Info } from 'lucide-react'
import { Layout } from '@/components/Layout'
import { Modal } from '@/components/Modal'
import { Badge } from '@/components/Badge'
import { StatCard } from '@/components/StatCard'
import {
  FormField,
  FormSection,
  inputClass,
  ModalActions,
  ErrorBox,
  ConfirmDelete,
  useStateReset,
} from '@/components/FormHelpers'
import {
  PageHeader,
  Card,
  CardHeader,
  CardBody,
  TableCard,
  Table,
  THead,
  TH,
  TBody,
  TR,
  TD,
  RowActions,
  Button,
  IconButton,
  StatusBadge,
  StatusKind,
  Tooltip,
  TableSkeleton,
  CopyField,
  useToast,
} from '@/components/ui'
import { getCluster, listServers, addServer, deleteServer, getRole, ClusterInfo, SavedServer } from '@/lib/api'

export default function ClusterPage() {
  return (
    <Layout>
      <ServersContent />
    </Layout>
  )
}

/** One table row: a node seen in the cluster, a server added by hand, or both. */
interface Row {
  key: string
  name: string
  url: string
  self: boolean
  discovered: boolean
  saved?: SavedServer
  reachable?: boolean
  priority?: number
  liveStreams?: number
  ndvrStreams?: number
  ndvrPublishing?: number
  lastSeen?: string
}

// URLs are matched on scheme + host + port, ignoring case and a trailing
// slash, so "http://Origin-2:8080/" and "http://origin-2:8080" are the same
// server.
function urlKey(u: string): string {
  try {
    const p = new URL(u)
    return `${p.protocol}//${p.host}`.toLowerCase()
  } catch {
    return u.trim().replace(/\/+$/, '').toLowerCase()
  }
}

function safeHref(u: string): string | undefined {
  try {
    const p = new URL(u)
    return p.protocol === 'http:' || p.protocol === 'https:' ? p.origin : undefined
  } catch {
    return undefined
  }
}

function ago(iso?: string): string {
  if (!iso) return '—'
  const s = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000))
  if (s < 5) return 'just now'
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  return new Date(iso).toLocaleString()
}

function buildRows(cluster: ClusterInfo | undefined, saved: SavedServer[]): Row[] {
  const rows: Row[] = []
  const byUrl = new Map<string, Row>()
  for (const n of cluster?.nodes ?? []) {
    const r: Row = {
      key: n.self ? 'self' : n.node_id,
      name: n.node_name || '(unnamed)',
      url: n.url,
      self: n.self,
      discovered: !n.self,
      reachable: n.reachable,
      priority: cluster?.enabled ? n.priority : undefined,
      liveStreams: n.live_streams,
      ndvrStreams: n.ndvr_streams,
      ndvrPublishing: n.ndvr_publishing,
      lastSeen: n.self ? undefined : n.last_seen,
    }
    rows.push(r)
    if (n.url) byUrl.set(urlKey(n.url), r)
  }
  for (const s of saved) {
    if (s.id === 'local') continue
    const hit = byUrl.get(urlKey(s.url))
    if (hit) {
      hit.saved = s
      continue
    }
    rows.push({ key: 'saved-' + s.id, name: s.name, url: s.url, self: false, discovered: false, saved: s })
  }
  return rows
}

function rowStatus(r: Row, enabled: boolean): { kind: StatusKind; label: string; hint: string } {
  if (r.self) return { kind: 'online', label: 'Online', hint: 'This server.' }
  if (r.discovered) {
    return r.reachable
      ? { kind: 'online', label: 'Online', hint: 'Heartbeats arriving over NATS.' }
      : {
          kind: 'warning',
          label: 'Unreachable',
          hint: 'No heartbeat for 3 intervals. nDVR streams that depend on it answer 503 until it returns or is forgotten.',
        }
  }
  if (!enabled) {
    return { kind: 'idle', label: 'Unknown', hint: 'Cluster mode is off on this server, so it can’t see other servers.' }
  }
  return {
    kind: 'idle',
    label: 'Not in cluster',
    hint: 'No heartbeat from this URL. Enable [multinode] on it with the same NATS server and set its advertise_url to this URL.',
  }
}

function ServersContent() {
  const isAdmin = getRole() === 'admin'
  const toast = useToast()
  const { data: cluster, isLoading } = useSWR('cluster', getCluster, { refreshInterval: 5000 })
  const { data: saved, mutate: mutateSaved } = useSWR(isAdmin ? 'servers' : null, listServers, {
    refreshInterval: 30000,
  })
  const [addOpen, setAddOpen] = useState(false)
  const [removing, setRemoving] = useState<SavedServer | null>(null)

  const enabled = !!cluster?.enabled
  const rows = useMemo(() => buildRows(cluster, saved ?? []), [cluster, saved])
  const clusterNodes = cluster?.nodes ?? []
  const online = clusterNodes.filter((n) => n.reachable).length
  const liveTotal = clusterNodes.filter((n) => n.reachable).reduce((a, n) => a + n.live_streams, 0)

  const busLabel = !enabled ? 'Off' : cluster?.connected ? (cluster.ready ? 'Connected' : 'Syncing') : 'Disconnected'

  return (
    <>
      <PageHeader
        title="Servers"
        description="Origin servers in this cluster: discovered automatically over NATS, or added by hand."
        actions={
          isAdmin && (
            <Button variant="primary" icon={<Plus size={15} />} onClick={() => setAddOpen(true)}>
              Add server
            </Button>
          )
        }
      />

      <div className="grid grid-cols-2 gap-3 sm:gap-4 xl:grid-cols-4">
        <StatCard
          label="Mode"
          value={!cluster ? '—' : enabled ? 'Cluster' : 'Single node'}
          sub={enabled ? `ownership: ${cluster?.ownership_policy?.replace('_', ' ')}` : 'multinode disabled'}
          icon={<Network size={18} />}
          color="purple"
        />
        <StatCard
          label="Message bus"
          value={busLabel}
          sub={enabled ? cluster?.nats_url : 'NATS not used'}
          icon={<Activity size={18} />}
          color={!enabled ? 'blue' : cluster?.connected ? 'green' : 'red'}
        />
        <StatCard
          label="Servers online"
          value={cluster ? `${online} / ${clusterNodes.length}` : '—'}
          sub={enabled ? `${clusterNodes.length - online} unreachable` : 'this server only'}
          icon={<Server size={18} />}
          color={clusterNodes.length - online > 0 ? 'yellow' : 'green'}
        />
        <StatCard
          label="Live streams"
          value={cluster ? liveTotal : '—'}
          sub="across online servers"
          icon={<Radio size={18} />}
          color="blue"
        />
      </div>

      {cluster && !enabled && <EnableClusterCard />}

      {isLoading ? (
        <TableSkeleton rows={2} cols={6} />
      ) : (
        <TableCard
          footer={
            enabled
              ? `Heartbeat every ${cluster?.heartbeat_interval_s}s · unreachable servers are forgotten after ${cluster?.peer_forget_s}s`
              : undefined
          }
        >
          <Table dense>
            <THead>
              <tr>
                <TH>Server</TH>
                <TH>Status</TH>
                <TH>Source</TH>
                <TH align="right">Priority</TH>
                <TH align="right">Live</TH>
                <TH align="right">nDVR</TH>
                <TH>Last heartbeat</TH>
                <TH align="right">
                  <span className="sr-only">Actions</span>
                </TH>
              </tr>
            </THead>
            <TBody>
              {rows.map((r) => {
                const st = rowStatus(r, enabled)
                const href = safeHref(r.url)
                return (
                  <TR key={r.key}>
                    <TD>
                      <div className="flex items-center gap-2">
                        <span className="font-medium text-fg">{r.name}</span>
                        {r.self && <Badge variant="purple">this server</Badge>}
                      </div>
                      <div className="mt-0.5 max-w-[280px] truncate font-mono text-xs text-fg-muted" title={r.url}>
                        {r.url || '—'}
                      </div>
                      {r.saved?.note && <div className="mt-0.5 text-xs text-fg-muted">{r.saved.note}</div>}
                    </TD>
                    <TD>
                      <Tooltip content={st.hint}>
                        <span tabIndex={0} aria-label={`${st.label}: ${st.hint}`}>
                          <StatusBadge status={st.kind} label={st.label} />
                        </span>
                      </Tooltip>
                    </TD>
                    <TD muted className="whitespace-nowrap">
                      {r.self ? 'This server' : r.discovered ? (r.saved ? 'Discovered · added' : 'Discovered') : 'Added'}
                    </TD>
                    <TD numeric>{r.priority ?? '—'}</TD>
                    <TD numeric>{r.liveStreams ?? '—'}</TD>
                    <TD numeric>
                      {r.ndvrStreams === undefined || !enabled ? (
                        '—'
                      ) : (
                        <Tooltip content={`${r.ndvrPublishing} publishing, ${r.ndvrStreams} with a recording or live`}>
                          <span tabIndex={0}>
                            {r.ndvrPublishing}
                            <span className="text-fg-muted"> / {r.ndvrStreams}</span>
                          </span>
                        </Tooltip>
                      )}
                    </TD>
                    <TD muted className="whitespace-nowrap">
                      {r.self ? '—' : ago(r.lastSeen)}
                    </TD>
                    <TD align="right">
                      <RowActions>
                        {href && !r.self && (
                          <a
                            href={href}
                            target="_blank"
                            rel="noopener noreferrer"
                            aria-label={`Open ${r.name} panel`}
                            className="inline-flex h-7 w-7 items-center justify-center rounded-md text-fg-muted hover:bg-elevated hover:text-fg"
                          >
                            <ExternalLink size={14} />
                          </a>
                        )}
                        {isAdmin && r.saved && (
                          <IconButton label={`Remove ${r.name}`} size="xs" onClick={() => setRemoving(r.saved!)}>
                            <Trash2 size={14} />
                          </IconButton>
                        )}
                      </RowActions>
                    </TD>
                  </TR>
                )
              })}
            </TBody>
          </Table>
        </TableCard>
      )}

      <AddServerModal
        open={addOpen}
        onClose={() => setAddOpen(false)}
        onSaved={(s) => {
          setAddOpen(false)
          toast.success(`Added ${s.name}`)
          mutateSaved()
        }}
      />
      <ConfirmDelete
        open={!!removing}
        title="Remove server"
        message={`Remove "${removing?.name ?? ''}" from this list? This doesn't change the server itself; if it's in the cluster it will still be listed as discovered.`}
        onCancel={() => setRemoving(null)}
        onConfirm={async () => {
          if (!removing) return
          await deleteServer(removing.id)
          toast.success(`Removed ${removing.name}`)
          setRemoving(null)
          mutateSaved()
        }}
      />
    </>
  )
}

const ENABLE_SNIPPET = `[multinode]
enabled          = true
nats_url         = "nats://<nats-host>:4222"
advertise_url    = "http://<this-server>:8080"
ownership_policy = "first_publisher"
node_priority    = 1   # 1 on the primary, 2 on the next`

function EnableClusterCard() {
  return (
    <Card>
      <CardHeader
        icon={<Info size={18} />}
        title="Cluster mode is off"
        description="Servers find each other over NATS once multinode is enabled in each server's config.toml. Restart the service after editing."
      />
      <CardBody className="space-y-3">
        <pre className="overflow-x-auto rounded-md border border-border bg-canvas/60 p-3 font-mono text-xs leading-relaxed text-fg-secondary">
          {ENABLE_SNIPPET}
        </pre>
        <p className="text-xs text-fg-muted">
          Every server needs the same NATS server, a unique node name and its own advertise_url. Servers you add here
          are shown as &ldquo;Not in cluster&rdquo; until their heartbeats arrive.
        </p>
        <CopyField value="/etc/origin-mediaserver/config.toml" className="max-w-sm" />
      </CardBody>
    </Card>
  )
}

function AddServerModal({
  open,
  onClose,
  onSaved,
}: {
  open: boolean
  onClose: () => void
  onSaved: (s: SavedServer) => void
}) {
  const [form, setForm] = useState({ name: '', url: '', note: '' })
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  useStateReset(String(open), () => {
    if (!open) return
    setForm({ name: '', url: '', note: '' })
    setErr(null)
  })

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!safeHref(form.url)) {
      setErr('Enter an http(s) URL like http://origin-2.example.com:8080')
      return
    }
    setBusy(true)
    setErr(null)
    try {
      onSaved(await addServer(form))
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
      description="Keep another origin in this list. It shows as online once it joins the cluster with the same URL as its advertise_url."
    >
      <form onSubmit={onSubmit} className="space-y-4">
        <FormSection title="Server">
          <FormField label="Name" required>
            <input
              required
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
              className={inputClass}
              placeholder="origin-bom-ovh02"
            />
          </FormField>
          <FormField label="URL" required hint="The server's base URL, as set in its [multinode] advertise_url.">
            <input
              required
              value={form.url}
              onChange={(e) => setForm({ ...form, url: e.target.value })}
              className={inputClass + ' font-mono'}
              placeholder="http://148.113.1.171:8080"
              inputMode="url"
            />
          </FormField>
          <FormField label="Note">
            <input
              value={form.note}
              onChange={(e) => setForm({ ...form, note: e.target.value })}
              className={inputClass}
              placeholder="Backup origin, Mumbai"
            />
          </FormField>
        </FormSection>
        <ErrorBox msg={err} />
        <ModalActions busy={busy} onCancel={onClose} submitLabel="Add server" />
      </form>
    </Modal>
  )
}
