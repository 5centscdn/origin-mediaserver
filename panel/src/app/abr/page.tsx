'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { Plus, Trash2, Pencil, Layers, Copy, Check, X } from 'lucide-react'
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
import { formatBitrate, copyToClipboard, isStreamPattern, matchesStreamPattern } from '@/lib/utils'

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
  const { data: groups, mutate: refreshGroups } = useSWR(
    'abr-groups',
    listABRGroups,
    { refreshInterval: 10000 },
  )
  const { data: live } = useSWR('streams', listStreams, { refreshInterval: 5000 })
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [modal, setModal] = useState<{ open: boolean; edit: ABRGroup | null }>({
    open: false,
    edit: null,
  })
  const [deleteTarget, setDeleteTarget] = useState<ABRGroup | null>(null)
  const [copied, setCopied] = useState(false)

  const selected = (groups ?? []).find((g) => g.id === selectedId) ?? null

  const { data: tracks, mutate: refreshTracks } = useSWR(
    selected ? ['abr-variants', selected.id] : null,
    () => listABRVariants(selected!.id),
    { refreshInterval: 10000 },
  )

  // For a pattern track (test%i): live if any matching stream is.
  const isLive = (app: string, stream: string) =>
    (live ?? []).some((s) => s.app === app && matchesStreamPattern(stream, s.name))


  const copyURL = async () => {
    if (!selected) return
    const url = `${window.location.origin}/${selected.app_name}/${selected.stream_name}/playlist.m3u8`
    if (await copyToClipboard(url)) {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    }
  }

  return (
    <div className="space-y-6 max-w-7xl">
      <div>
        <h1 className="text-2xl font-semibold text-white">ABR Groups</h1>
        <p className="text-sm text-gray-500 mt-1">
          Combine streams into one multi-bitrate HLS master playlist
        </p>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
        <div className="space-y-3">
          <div className="flex items-center justify-between">
            <h2 className="text-xs font-medium text-gray-500 uppercase tracking-wider">
              Groups
            </h2>
            <button
              type="button"
              onClick={() => setModal({ open: true, edit: null })}
              className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-blue-600 hover:bg-blue-500 text-white transition"
            >
              <Plus size={14} />
              New
            </button>
          </div>

          {!groups || groups.length === 0 ? (
            <div className="rounded-lg border border-gray-800 bg-gray-900 p-6 text-center">
              <Layers className="mx-auto text-gray-600 mb-2" size={24} />
              <div className="text-sm text-gray-400">No ABR groups</div>
              <div className="text-xs text-gray-500 mt-1">
                Streams published as name%i (FMLE) are grouped automatically
              </div>
            </div>
          ) : (
            <div className="space-y-2">
              {groups.map((g) => (
                <button
                  key={g.id}
                  type="button"
                  onClick={() => setSelectedId(g.id)}
                  className={
                    'w-full text-left rounded-lg border px-3 py-2.5 transition ' +
                    (selectedId === g.id
                      ? 'border-blue-500 bg-blue-500/10'
                      : 'border-gray-800 bg-gray-900 hover:border-gray-700')
                  }
                >
                  <div className="text-sm font-medium text-white truncate">
                    {g.app_name}/{g.stream_name}
                  </div>
                </button>
              ))}
            </div>
          )}
        </div>

        <div className="lg:col-span-2">
          {selected ? (
            <div className="space-y-4">
              <div className="rounded-lg border border-gray-800 bg-gray-900 p-4">
                <div className="flex items-start justify-between gap-3">
                  <div className="min-w-0">
                    <h2 className="text-base font-semibold text-white truncate">
                      {selected.app_name}/{selected.stream_name}
                    </h2>
                    <div className="mt-2 flex items-center gap-2 flex-wrap">
                      <span className="text-xs text-gray-500">Live URL:</span>
                      <code className="text-xs text-gray-300 font-mono bg-gray-800 px-2 py-0.5 rounded break-all">
                        /{selected.app_name}/{selected.stream_name}/playlist.m3u8
                      </code>
                      <button
                        type="button"
                        onClick={copyURL}
                        className="text-gray-400 hover:text-white transition"
                        title="Copy"
                      >
                        {copied ? (
                          <Check size={14} className="text-green-400" />
                        ) : (
                          <Copy size={14} />
                        )}
                      </button>
                    </div>
                  </div>
                  <div className="flex gap-1.5 flex-shrink-0">
                    <button
                      type="button"
                      onClick={() => setModal({ open: true, edit: selected })}
                      className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
                    >
                      <Pencil size={12} />
                      Edit
                    </button>
                    <button
                      type="button"
                      onClick={() => setDeleteTarget(selected)}
                      className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-red-600/20 hover:bg-red-600/30 text-red-400 border border-red-600/40 transition"
                    >
                      <Trash2 size={12} />
                      Delete
                    </button>
                  </div>
                </div>
              </div>

              <div className="rounded-lg border border-gray-800 bg-gray-900 overflow-hidden">
                <div className="px-4 py-3 border-b border-gray-800">
                  <h3 className="text-sm font-medium text-gray-200">
                    Tracks ({tracks?.length ?? 0})
                  </h3>
                </div>
                {!tracks || tracks.length === 0 ? (
                  <div className="px-4 py-8 text-center text-sm text-gray-500">
                    No tracks yet. Click Edit to add some.
                  </div>
                ) : (
                  <div className="overflow-x-auto">
                    <table className="w-full text-sm">
                      <thead className="bg-gray-800/40 text-xs uppercase tracking-wider text-gray-500">
                        <tr>
                          <th className="px-4 py-2 text-left font-medium">Stream</th>
                          <th className="px-4 py-2 text-left font-medium">Status</th>
                          <th className="px-4 py-2 text-left font-medium">Bandwidth</th>
                          <th className="px-4 py-2 text-left font-medium">Avg</th>
                          <th className="px-4 py-2 text-left font-medium">Resolution</th>
                          <th className="px-4 py-2 text-left font-medium">Codecs</th>
                        </tr>
                      </thead>
                      <tbody className="divide-y divide-gray-800">
                        {tracks.map((v) => (
                          <tr key={v.id}>
                            <td className="px-4 py-2.5 text-white font-mono text-xs">
                              {v.stream_name}
                              {(v.stream_name.includes('%i') || v.stream_name.includes('*')) && (
                                <span className="ml-2">
                                  <Badge variant="purple">every match</Badge>
                                </span>
                              )}
                              {v.audio_only && (
                                <span className="ml-2">
                                  <Badge variant="purple">audio</Badge>
                                </span>
                              )}
                            </td>
                            <td className="px-4 py-2.5">
                              {isLive(v.src_app_name || selected.app_name, v.stream_name) ? (
                                <Badge variant="green">live</Badge>
                              ) : (
                                <Badge variant="gray">offline</Badge>
                              )}
                            </td>
                            <td className="px-4 py-2.5 text-gray-300">
                              {v.bandwidth ? formatBitrate(v.bandwidth) : 'auto'}
                            </td>
                            <td className="px-4 py-2.5 text-gray-300">
                              {v.avg_bandwidth ? formatBitrate(v.avg_bandwidth) : '—'}
                            </td>
                            <td className="px-4 py-2.5 text-gray-300">
                              {v.width && v.height ? `${v.width}x${v.height}` : 'auto'}
                            </td>
                            <td className="px-4 py-2.5 text-gray-400 text-xs font-mono">
                              {v.codecs || 'auto'}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </div>
            </div>
          ) : (
            <div className="rounded-lg border border-gray-800 bg-gray-900 p-12 text-center">
              <Layers className="mx-auto text-gray-600 mb-3" size={32} />
              <div className="text-sm text-gray-400">Select a group</div>
            </div>
          )}
        </div>
      </div>

      <GroupModal
        open={modal.open}
        edit={modal.edit}
        onClose={() => setModal({ open: false, edit: null })}
        onSaved={(id) => {
          setModal({ open: false, edit: null })
          setSelectedId(id)
          refreshGroups()
          refreshTracks()
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
          if (deleteTarget.id === selectedId) setSelectedId(null)
          setDeleteTarget(null)
          refreshGroups()
        }}
      />
    </div>
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

  const cell = inputClass + ' !py-1.5 text-sm disabled:opacity-50 disabled:cursor-not-allowed'

  return (
    <Modal
      open={open}
      onClose={() => !busy && onClose()}
      title={edit ? `Edit ABR Group: ${edit.app_name}/${edit.stream_name}` : 'New ABR Group'}
      size="xl"
    >
      <form onSubmit={onSubmit} className="space-y-4">
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <FormField label="App Name">
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
            hint={appName && groupName ? `/${appName}/${groupName}/playlist.m3u8` : 'Used in the playback URL'}
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

        <div>
          <div className="flex items-center justify-between mb-2">
            <div className="text-xs uppercase tracking-wider text-gray-500">Tracks</div>
            <button
              type="button"
              onClick={() => setTracks([...tracks, emptyTrack()])}
              className="inline-flex items-center gap-1 px-2.5 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-200 border border-gray-700 transition"
            >
              <Plus size={14} />
              Add Track
            </button>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-xs uppercase tracking-wider text-gray-500">
                <tr className="border-b border-gray-800">
                  <th className="px-2 py-2 text-left font-medium">Stream Name</th>
                  <th className="px-2 py-2 text-left font-medium">Bandwidth (bps)</th>
                  <th className="px-2 py-2 text-left font-medium">Avg Bandwidth</th>
                  <th className="px-2 py-2 text-left font-medium">Resolution</th>
                  <th className="px-2 py-2 text-left font-medium">Codecs</th>
                  <th className="px-2 py-2 text-center font-medium">Audio Only</th>
                  <th />
                </tr>
              </thead>
              <tbody className="divide-y divide-gray-800">
                {tracks.map((t, i) => (
                  <tr key={i}>
                    <td className="px-2 py-2 min-w-[180px]">
                      <select
                        required
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
                        <div className="mt-1 text-[11px] text-purple-300">
                          {liveMatches(t.stream_name).length
                            ? `live now: ${liveMatches(t.stream_name).join(', ')}`
                            : 'none live now; renditions appear as they publish'}
                        </div>
                      )}
                    </td>
                    <td className="px-2 py-2 min-w-[120px]">
                      <input
                        type="number"
                        min={0}
                        value={isPattern(t.stream_name) ? '' : t.bandwidth || ''}
                        onChange={(e) => setTrack(i, { bandwidth: Number(e.target.value) || 0 })}
                        className={cell}
                        disabled={isPattern(t.stream_name)}
                        placeholder={isPattern(t.stream_name) ? 'auto per stream' : '0=auto'}
                      />
                    </td>
                    <td className="px-2 py-2 min-w-[120px]">
                      <input
                        type="number"
                        min={0}
                        value={isPattern(t.stream_name) ? '' : t.avg_bandwidth || ''}
                        onChange={(e) =>
                          setTrack(i, { avg_bandwidth: Number(e.target.value) || 0 })
                        }
                        className={cell}
                        disabled={isPattern(t.stream_name)}
                        placeholder={isPattern(t.stream_name) ? 'auto' : '0=omit'}
                      />
                    </td>
                    <td className="px-2 py-2 min-w-[110px]">
                      <input
                        value={isPattern(t.stream_name) ? '' : t.resolution}
                        onChange={(e) => setTrack(i, { resolution: e.target.value })}
                        className={cell}
                        disabled={isPattern(t.stream_name)}
                        placeholder={isPattern(t.stream_name) ? 'auto per stream' : 'auto'}
                        pattern="\d+[xX]\d+"
                        title="WIDTHxHEIGHT, e.g. 1280x720, or empty for auto"
                      />
                    </td>
                    <td className="px-2 py-2 min-w-[160px]">
                      <input
                        value={t.codecs}
                        onChange={(e) => setTrack(i, { codecs: e.target.value })}
                        className={cell}
                        placeholder="auto"
                      />
                    </td>
                    <td className="px-2 py-2 text-center">
                      <input
                        type="checkbox"
                        checked={t.audio_only}
                        onChange={(e) => setTrack(i, { audio_only: e.target.checked })}
                      />
                    </td>
                    <td className="px-2 py-2 text-right">
                      <button
                        type="button"
                        disabled={tracks.length === 1}
                        onClick={() => setTracks(tracks.filter((_, j) => j !== i))}
                        className="px-2 py-1 rounded bg-red-600/20 hover:bg-red-600/30 text-red-400 border border-red-600/40 transition disabled:opacity-40"
                        title="Remove track"
                      >
                        <X size={14} />
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="text-xs text-gray-500 mt-2">
            Lists {appName || "the app"}&apos;s streams (create them under{' '}
            <a href="/stream-configs/" className="text-blue-400 hover:text-blue-300">
              Streams
            </a>
            , e.g. test1…test5) plus anything live now. Offline tracks are left out of the master
            until they publish.
            {patterns.length > 0 && (
              <>
                {' '}Choose <span className="font-mono text-gray-400">{patterns[0]}</span> to include every
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
