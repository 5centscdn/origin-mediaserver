'use client'

import { useEffect, useState } from 'react'
import useSWR from 'swr'
import clsx from 'clsx'
import { Plus, Trash2, Pencil, Layers, X, ExternalLink, ListPlus } from 'lucide-react'
import { Layout } from '@/components/Layout'
import { Modal } from '@/components/Modal'
import { Badge } from '@/components/Badge'
import {
  FormField,
  inputClass,
  checkboxClass,
  ModalActions,
  ErrorBox,
  ConfirmDelete,
  useStateReset,
} from '@/components/FormHelpers'
import {
  PageHeader,
  Card,
  CardHeader,
  DescriptionList,
  DescriptionRow,
  TableCard,
  Table,
  THead,
  TH,
  TBody,
  TR,
  TD,
  Button,
  IconButton,
  CopyField,
  EmptyState,
  Skeleton,
  TableSkeleton,
  StatusBadge,
  buttonClass,
  useToast,
} from '@/components/ui'
import {
  listABRGroups,
  createABRGroup,
  updateABRGroup,
  deleteABRGroup,
  listABRVariants,
  replaceABRTracks,
  listApplications,
  listStreamConfigs,
  listStreams,
  ABRGroup,
  ABRTrack,
  ABRVariant,
} from '@/lib/api'
import { formatBitrate, isStreamPattern, matchesStreamPattern } from '@/lib/utils'

// ABR groups follow the standalone DVR server's editor: a group is an app
// plus a name (served at /{app}/{name}/playlist.m3u8) and an ordered list of
// tracks, each a stream in that app. Bandwidth 0, an empty resolution and
// empty codecs are auto-detected from the live stream; offline tracks are
// left out of the master (see internal/api/abr_master.go).

export default function ABRPage() {
  return (
    <Layout>
      <ABRContent />
    </Layout>
  )
}

function ABRContent() {
  const { data: groups, mutate: refreshGroups, isLoading } = useSWR(
    'abr-groups',
    listABRGroups,
    { refreshInterval: 10000 },
  )
  const { data: live } = useSWR('streams', listStreams, { refreshInterval: 5000 })
  const toast = useToast()
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [modal, setModal] = useState<{ open: boolean; edit: ABRGroup | null }>({
    open: false,
    edit: null,
  })
  const [deleteTarget, setDeleteTarget] = useState<ABRGroup | null>(null)

  const selected = (groups ?? []).find((g) => g.id === selectedId) ?? null
  // Open the first group by default (and after the selected one is deleted)
  // instead of an empty detail pane.
  useEffect(() => {
    if (!selected && groups && groups.length > 0) setSelectedId(groups[0].id)
  }, [groups, selected])

  const { data: tracks, mutate: refreshTracks } = useSWR(
    selected ? ['abr-variants', selected.id] : null,
    () => listABRVariants(selected!.id),
    { refreshInterval: 10000 },
  )

  // For a pattern track (test%i): live if any matching stream is.
  const isLive = (app: string, stream: string) =>
    (live ?? []).some((s) => s.app === app && matchesStreamPattern(stream, s.name))

  const origin = typeof window !== 'undefined' ? window.location.origin : ''
  const playbackPath = (g: ABRGroup) => `/${g.app_name}/${g.stream_name}/playlist.m3u8`

  const openCreate = () => setModal({ open: true, edit: null })
  const openEdit = (g: ABRGroup) => setModal({ open: true, edit: g })

  const liveTracks = (tracks ?? []).filter((v) =>
    selected ? isLive(v.src_app_name || selected.app_name, v.stream_name) : false,
  ).length

  return (
    <>
      <PageHeader
        title="ABR Groups"
        description="Combine streams into one multi-bitrate HLS master playlist."
        actions={
          <Button variant="primary" size="md" icon={<Plus size={16} />} onClick={openCreate}>
            New ABR Group
          </Button>
        }
      />

      {isLoading ? (
        <div className="grid grid-cols-1 gap-6 lg:grid-cols-[300px_minmax(0,1fr)]">
          <Card className="space-y-2 p-3">
            <Skeleton className="h-9 w-full" />
            <Skeleton className="h-9 w-full" />
            <Skeleton className="h-9 w-full" />
          </Card>
          <TableSkeleton rows={3} cols={5} />
        </div>
      ) : !groups || groups.length === 0 ? (
        <Card>
          <EmptyState
            icon={<Layers size={20} />}
            title="No ABR groups"
            description="Create a group to combine renditions into one master playlist. Streams published as name%i (FMLE) are grouped automatically."
            action={
              <Button variant="primary" size="md" icon={<Plus size={16} />} onClick={openCreate}>
                New ABR Group
              </Button>
            }
          />
        </Card>
      ) : (
        <div className="grid grid-cols-1 items-start gap-6 lg:grid-cols-[300px_minmax(0,1fr)]">
          {/* Group list */}
          <Card className="overflow-hidden">
            <div className="flex items-center justify-between gap-2 border-b border-border px-4 py-3">
              <h2 className="text-[15px] font-semibold text-fg">
                Groups <span className="font-normal text-fg-muted">({groups.length})</span>
              </h2>
              <Button variant="secondary" size="xs" icon={<Plus size={13} />} onClick={openCreate}>
                New
              </Button>
            </div>
            <ul className="divide-y divide-border">
              {groups.map((g) => {
                const active = selectedId === g.id
                return (
                  <li key={g.id}>
                    <button
                      type="button"
                      onClick={() => setSelectedId(g.id)}
                      aria-current={active ? 'true' : undefined}
                      className={clsx(
                        'block w-full border-l-2 px-4 py-2.5 text-left transition-colors',
                        'focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary-fg',
                        active ? 'border-primary bg-primary/10' : 'border-transparent hover:bg-elevated/60',
                      )}
                    >
                      <div className={clsx('truncate text-sm font-medium', active ? 'text-fg' : 'text-fg-secondary')}>
                        {g.stream_name}
                      </div>
                      <div className="truncate font-mono text-xs text-fg-muted">
                        {g.app_name}/{g.stream_name}
                      </div>
                    </button>
                  </li>
                )
              })}
            </ul>
          </Card>

          {/* Detail */}
          {selected ? (
            <div className="min-w-0 space-y-6">
              <Card>
                <CardHeader
                  icon={<Layers size={18} />}
                  title={`${selected.app_name}/${selected.stream_name}`}
                  description="Manual ABR group (HLS)"
                  actions={
                    <>
                      <Button variant="secondary" size="sm" icon={<Pencil size={14} />} onClick={() => openEdit(selected)}>
                        Edit
                      </Button>
                      <Button
                        variant="danger-outline"
                        size="sm"
                        icon={<Trash2 size={14} />}
                        onClick={() => setDeleteTarget(selected)}
                      >
                        Delete
                      </Button>
                    </>
                  }
                />
                <DescriptionList>
                  <DescriptionRow label="Playback URL" hint="HLS master playlist">
                    <div className="flex min-w-0 items-center gap-2">
                      <CopyField value={`${origin}${playbackPath(selected)}`} className="min-w-0 flex-1" />
                      <a
                        href={playbackPath(selected)}
                        target="_blank"
                        rel="noreferrer"
                        className={buttonClass('ghost', 'xs', 'shrink-0')}
                        title="Open master playlist"
                      >
                        <ExternalLink size={13} />
                        Open
                      </a>
                    </div>
                  </DescriptionRow>
                  <DescriptionRow label="Variants">
                    {tracks ? (
                      <span className="tabular">
                        {tracks.length} configured · {liveTracks} live
                      </span>
                    ) : (
                      <span className="text-fg-muted">—</span>
                    )}
                  </DescriptionRow>
                </DescriptionList>
              </Card>

              {!tracks ? (
                <TableSkeleton rows={3} cols={6} />
              ) : tracks.length === 0 ? (
                <Card>
                  <EmptyState
                    compact
                    icon={<ListPlus size={20} />}
                    title="No variants yet"
                    description="Add the renditions (streams) this group's master playlist should offer."
                    action={
                      <Button variant="primary" size="sm" icon={<Plus size={14} />} onClick={() => openEdit(selected)}>
                        Add variants
                      </Button>
                    }
                  />
                </Card>
              ) : (
                <TableCard
                  header={
                    <div className="flex w-full flex-wrap items-center justify-between gap-2">
                      <h3 className="text-[15px] font-semibold text-fg">
                        Variants <span className="font-normal text-fg-muted">({tracks.length})</span>
                      </h3>
                      <Button variant="secondary" size="xs" icon={<Pencil size={13} />} onClick={() => openEdit(selected)}>
                        Add / edit variants
                      </Button>
                    </div>
                  }
                  footer="Empty values are auto-detected from the live stream. Offline variants are left out of the master."
                >
                  <Table>
                    <THead>
                      <tr>
                        <TH>Stream</TH>
                        <TH>Status</TH>
                        <TH align="right">Bandwidth</TH>
                        <TH align="right">Avg</TH>
                        <TH align="right">Resolution</TH>
                        <TH>Codecs</TH>
                      </tr>
                    </THead>
                    <TBody>
                      {tracks.map((v) => {
                        const srcApp = v.src_app_name || selected.app_name
                        const on = isLive(srcApp, v.stream_name)
                        return (
                          <TR key={v.id}>
                            <TD>
                              <div className="flex flex-wrap items-center gap-2">
                                <span className="font-mono text-xs text-fg">
                                  {v.src_app_name && v.src_app_name !== selected.app_name && (
                                    <span className="text-fg-muted">{v.src_app_name}/</span>
                                  )}
                                  {v.stream_name}
                                </span>
                                {(v.stream_name.includes('%i') || v.stream_name.includes('*')) && (
                                  <Badge variant="purple">every match</Badge>
                                )}
                                {v.audio_only && <Badge variant="purple">audio</Badge>}
                              </div>
                            </TD>
                            <TD>
                              <StatusBadge status={on ? 'live' : 'idle'} label={on ? 'Live' : 'Offline'} />
                            </TD>
                            <TD numeric>{v.bandwidth ? formatBitrate(v.bandwidth) : <span className="text-fg-muted">auto</span>}</TD>
                            <TD numeric>{v.avg_bandwidth ? formatBitrate(v.avg_bandwidth) : <span className="text-fg-muted">—</span>}</TD>
                            <TD numeric>
                              {v.width && v.height ? `${v.width}x${v.height}` : <span className="text-fg-muted">auto</span>}
                            </TD>
                            <TD mono>{v.codecs || <span className="text-fg-muted">auto</span>}</TD>
                          </TR>
                        )
                      })}
                    </TBody>
                  </Table>
                </TableCard>
              )}
            </div>
          ) : (
            <Card>
              <EmptyState
                icon={<Layers size={20} />}
                title="Select a group"
                description="Pick a group on the left to see its playback URL and variants."
              />
            </Card>
          )}
        </div>
      )}

      <GroupModal
        open={modal.open}
        edit={modal.edit}
        onClose={() => setModal({ open: false, edit: null })}
        onSaved={(id) => {
          const wasEdit = !!modal.edit
          setModal({ open: false, edit: null })
          setSelectedId(id)
          refreshGroups()
          refreshTracks()
          toast.success(wasEdit ? 'ABR group saved' : 'ABR group created')
        }}
      />

      <ConfirmDelete
        open={!!deleteTarget}
        title="Delete ABR group"
        message={`Delete group "${deleteTarget ? `${deleteTarget.app_name}/${deleteTarget.stream_name}` : ''}"? Its playlist URL stops working; the streams themselves are not affected.`}
        onCancel={() => setDeleteTarget(null)}
        onConfirm={async () => {
          if (!deleteTarget) return
          await deleteABRGroup(deleteTarget.id)
          toast.success(`Deleted ABR group ${deleteTarget.app_name}/${deleteTarget.stream_name}`)
          if (deleteTarget.id === selectedId) setSelectedId(null)
          setDeleteTarget(null)
          refreshGroups()
        }}
      />
    </>
  )
}

const emptyTrack = (): ABRTrack => ({
  stream_name: '',
  bandwidth: 0,
  avg_bandwidth: 0,
  resolution: '',
  codecs: '',
  audio_only: false,
})

const trackFromVariant = (v: ABRVariant): ABRTrack => ({
  stream_name: v.stream_name,
  bandwidth: v.bandwidth,
  avg_bandwidth: v.avg_bandwidth,
  resolution: v.width && v.height ? `${v.width}x${v.height}` : '',
  codecs: v.codecs,
  audio_only: v.audio_only,
})

function GroupModal({
  open,
  edit,
  onClose,
  onSaved,
}: {
  open: boolean
  edit: ABRGroup | null
  onClose: () => void
  onSaved: (id: string) => void
}) {
  const [appName, setAppName] = useState('')
  const [groupName, setGroupName] = useState('')
  const [tracks, setTracks] = useState<ABRTrack[]>([emptyTrack()])
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const { data: apps } = useSWR(open ? 'applications' : null, listApplications)
  const app = (apps ?? []).find((a) => a.name === appName)
  const { data: configs } = useSWR(
    open && app ? ['stream-configs', app.id] : null,
    () => listStreamConfigs(app!.id),
  )
  const { data: live } = useSWR(open ? 'streams' : null, listStreams)

  // Stream choices for the chosen app: its live streams plus its
  // exact-name stream configs (a %i/wildcard config has no fixed names, so
  // its streams appear once live). A track's saved stream stays selectable
  // even while offline.
  const liveNames = new Set((live ?? []).filter((s) => s.app === appName).map((s) => s.name))
  const known = Array.from(
    new Set([
      ...Array.from(liveNames),
      ...(configs ?? []).filter((c) => !c.is_wildcard).map((c) => c.name_pattern),
    ]),
  ).sort()
  const streamOptions = (current: string) =>
    (current && !known.includes(current) ? [current, ...known] : known).map((name) => ({
      name,
      live: liveNames.has(name),
    }))
  const patterns = (configs ?? []).filter((c) => c.is_wildcard).map((c) => c.name_pattern)
  // A pattern track (test%i) stands for every matching stream; the master
  // expands it to whatever is live (internal/api.expandPatternTracks).
  const isPattern = isStreamPattern
  const liveMatches = (pat: string) =>
    Array.from(liveNames)
      .filter((n) => matchesStreamPattern(pat, n))
      .sort((a, b) => a.localeCompare(b, undefined, { numeric: true }))


  useStateReset(JSON.stringify({ open, eid: edit?.id ?? null }), () => {
    if (!open) return
    setErr(null)
    if (edit) {
      setAppName(edit.app_name)
      setGroupName(edit.stream_name)
      setTracks([])
      listABRVariants(edit.id)
        .then((vs) => setTracks(vs.length ? vs.map(trackFromVariant) : [emptyTrack()]))
        .catch((e) => setErr(e?.message ?? 'Failed to load tracks'))
    } else {
      setAppName('')
      setGroupName('')
      setTracks([emptyTrack()])
    }
  })

  const setTrack = (i: number, patch: Partial<ABRTrack>) =>
    setTracks(tracks.map((t, j) => (j === i ? { ...t, ...patch } : t)))

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr(null)
    const group = {
      name: `${appName}/${groupName}`,
      app_name: appName,
      stream_name: groupName,
      auto_detect: false,
    }
    let createdId: string | null = null
    try {
      let id: string
      if (edit) {
        id = edit.id
        await updateABRGroup(id, group)
      } else {
        id = (await createABRGroup(group)).id
        createdId = id
      }
      await replaceABRTracks(id, tracks)
      onSaved(id)
    } catch (e: any) {
      // Don't leave a new, trackless group behind when its tracks failed
      // validation; the user fixes the row and saves again.
      if (createdId) await deleteABRGroup(createdId).catch(() => {})
      setErr(e?.message ?? 'Save failed')
    } finally {
      setBusy(false)
    }
  }


  const cell = inputClass + ' disabled:opacity-50 disabled:cursor-not-allowed'
  const th = 'whitespace-nowrap px-2 py-2 text-left text-xs font-medium uppercase tracking-wide text-fg-muted'

  return (
    <Modal
      open={open}
      onClose={() => !busy && onClose()}
      title={edit ? `Edit ABR Group: ${edit.app_name}/${edit.stream_name}` : 'New ABR Group'}
      description="Pick the application and the streams (renditions) the master playlist offers. Changes are saved together."
      size="xl"
    >
      <form onSubmit={onSubmit} className="space-y-5">
        <div className="grid max-w-2xl grid-cols-1 gap-3 sm:grid-cols-2">
          <FormField label="App Name" required>
            <select
              required
              value={appName}
              onChange={(e) => setAppName(e.target.value)}
              className={inputClass}
            >
              <option value="" disabled>
                Select application
              </option>
              {(apps ?? []).map((a) => (
                <option key={a.id} value={a.name}>
                  {a.name}
                </option>
              ))}
              {appName && !app && <option value={appName}>{appName}</option>}
            </select>
          </FormField>
          <FormField
            label="Group Name"
            required
            hint={
              appName && groupName ? (
                <span className="font-mono">/{appName}/{groupName}/playlist.m3u8</span>
              ) : (
                'Used in the playback URL'
              )
            }
          >
            <input
              required
              value={groupName}
              onChange={(e) => setGroupName(e.target.value.replace(/\//g, ''))}
              className={inputClass}
              placeholder="abr"
            />
          </FormField>
        </div>

        <div className="space-y-2 border-t border-border pt-4">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div>
              <div className="text-[13px] font-semibold text-fg">
                Tracks <span className="font-normal text-fg-muted">({tracks.length})</span>
              </div>
              <div className="mt-0.5 text-xs text-fg-muted">Leave bandwidth, resolution and codecs empty to auto-detect.</div>
            </div>
            <Button
              variant="secondary"
              size="sm"
              icon={<Plus size={14} />}
              onClick={() => setTracks([...tracks, emptyTrack()])}
            >
              Add Track
            </Button>
          </div>
          <div className="overflow-x-auto rounded-md border border-border">
            <table className="w-full text-[13px]">
              <thead className="border-b border-border bg-elevated/40">
                <tr>
                  <th className={th}>Stream Name</th>
                  <th className={th}>Bandwidth (bps)</th>
                  <th className={th}>Avg Bandwidth</th>
                  <th className={th}>Resolution</th>
                  <th className={th}>Codecs</th>
                  <th className={th + ' !text-center'}>Audio Only</th>
                  <th className={th}>
                    <span className="sr-only">Remove</span>
                  </th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {tracks.map((t, i) => (
                  <tr key={i} className="align-top">
                    <td className="min-w-[200px] px-2 py-2">
                      <select
                        required
                        aria-label={`Track ${i + 1} stream`}
                        value={t.stream_name}
                        onChange={(e) => {
                          const v = e.target.value
                          setTrack(
                            i,
                            isPattern(v)
                              ? { stream_name: v, bandwidth: 0, avg_bandwidth: 0, resolution: '' }
                              : { stream_name: v },
                          )
                        }}
                        className={cell}
                      >
                        <option value="">
                          {appName ? '— select a stream —' : '— select an app first —'}
                        </option>
                        {patterns.length > 0 && (
                          <optgroup label="All matching streams (auto)">
                            {patterns.map((p) => (
                              <option key={p} value={p}>
                                {appName}/{p} — every match
                              </option>
                            ))}
                          </optgroup>
                        )}
                        <optgroup label="Single stream">
                          {streamOptions(t.stream_name)
                            .filter((o) => !isPattern(o.name) || !patterns.includes(o.name))
                            .map((o) => (
                              <option key={o.name} value={o.name}>
                                {appName}/{o.name}
                                {o.live ? ' (live)' : ''}
                              </option>
                            ))}
                        </optgroup>
                      </select>
                      {isPattern(t.stream_name) && (
                        <div className="mt-1 text-xs text-info">
                          {liveMatches(t.stream_name).length
                            ? `live now: ${liveMatches(t.stream_name).join(', ')}`
                            : 'none live now; renditions appear as they publish'}
                        </div>
                      )}
                    </td>
                    <td className="min-w-[130px] px-2 py-2">
                      <input
                        type="number"
                        min={0}
                        aria-label={`Track ${i + 1} bandwidth (bps)`}
                        value={isPattern(t.stream_name) ? '' : t.bandwidth || ''}
                        onChange={(e) => setTrack(i, { bandwidth: Number(e.target.value) || 0 })}
                        className={cell + ' tabular text-right'}
                        disabled={isPattern(t.stream_name)}
                        placeholder={isPattern(t.stream_name) ? 'auto per stream' : '0=auto'}
                      />
                    </td>
                    <td className="min-w-[130px] px-2 py-2">
                      <input
                        type="number"
                        min={0}
                        aria-label={`Track ${i + 1} average bandwidth`}
                        value={isPattern(t.stream_name) ? '' : t.avg_bandwidth || ''}
                        onChange={(e) =>
                          setTrack(i, { avg_bandwidth: Number(e.target.value) || 0 })
                        }
                        className={cell + ' tabular text-right'}
                        disabled={isPattern(t.stream_name)}
                        placeholder={isPattern(t.stream_name) ? 'auto' : '0=omit'}
                      />
                    </td>
                    <td className="min-w-[120px] px-2 py-2">
                      <input
                        aria-label={`Track ${i + 1} resolution`}
                        value={isPattern(t.stream_name) ? '' : t.resolution}
                        onChange={(e) => setTrack(i, { resolution: e.target.value })}
                        className={cell + ' font-mono text-xs'}
                        disabled={isPattern(t.stream_name)}
                        placeholder={isPattern(t.stream_name) ? 'auto per stream' : 'auto'}
                        pattern="\d+[xX]\d+"
                        title="WIDTHxHEIGHT, e.g. 1280x720, or empty for auto"
                      />
                    </td>
                    <td className="min-w-[170px] px-2 py-2">
                      <input
                        aria-label={`Track ${i + 1} codecs`}
                        value={t.codecs}
                        onChange={(e) => setTrack(i, { codecs: e.target.value })}
                        className={cell + ' font-mono text-xs'}
                        placeholder="auto"
                      />
                    </td>
                    <td className="px-2 py-2 text-center">
                      <div className="flex h-9 items-center justify-center">
                        <input
                          type="checkbox"
                          aria-label={`Track ${i + 1} audio only`}
                          className={checkboxClass}
                          checked={t.audio_only}
                          onChange={(e) => setTrack(i, { audio_only: e.target.checked })}
                        />
                      </div>
                    </td>
                    <td className="px-2 py-2 text-right">
                      <IconButton
                        variant="danger-outline"
                        size="sm"
                        label={tracks.length === 1 ? 'A group needs at least one track' : `Remove track ${i + 1}`}
                        disabled={tracks.length === 1}
                        onClick={() => setTracks(tracks.filter((_, j) => j !== i))}
                        className="mt-0.5"
                      >
                        <X size={14} />
                      </IconButton>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="max-w-3xl text-xs leading-relaxed text-fg-muted">
            Lists {appName || "the app"}&apos;s streams (create them under{' '}
            <a href="/stream-configs/" className="text-primary-fg hover:underline">
              Streams
            </a>
            , e.g. test1…test5) plus anything live now. Offline tracks are left out of the master
            until they publish.
            {patterns.length > 0 && (
              <>
                {' '}Choose <span className="font-mono text-fg-secondary">{patterns[0]}</span> to include every
                matching stream automatically (test1, test2, … as they publish); bandwidth and
                resolution then come from each stream.
              </>
            )}
          </div>
        </div>

        <ErrorBox msg={err} />
        <ModalActions
          busy={busy}
          onCancel={onClose}
          submitLabel={edit ? 'Save Changes' : 'Create'}
        />
      </form>
    </Modal>
  )
}
