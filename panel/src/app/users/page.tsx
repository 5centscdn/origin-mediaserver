'use client'

import { useState } from 'react'
import useSWR from 'swr'
import {
  Plus,
  Trash2,
  Pencil,
  Key,
  Users as UsersIcon,
  Copy,
  Check,
  AlertTriangle,
} from 'lucide-react'
import { Layout } from '@/components/Layout'
import { Modal } from '@/components/Modal'
import { Badge, BadgeVariant } from '@/components/Badge'
import {
  FormField,
  inputClass,
  ModalActions,
  ErrorBox,
  ConfirmDelete,
  useStateReset,
} from '@/components/FormHelpers'
import {
  listUsers,
  createUser,
  updateUser,
  deleteUser,
  listTokens,
  createToken,
  deleteToken,
  User,
  APIToken,
} from '@/lib/api'
import { formatDate, copyToClipboard } from '@/lib/utils'

const ROLES = ['admin', 'operator', 'viewer']
const ALL_SCOPES = ['read', 'write', 'stream']

function roleVariant(r: string): BadgeVariant {
  if (r === 'admin') return 'red'
  if (r === 'operator') return 'yellow'
  return 'gray'
}

export default function UsersPage() {
  return (
    <Layout>
      <UsersContent />
    </Layout>
  )
}

function UsersContent() {
  const { data, mutate } = useSWR('users', listUsers, { refreshInterval: 30000 })
  const [newOpen, setNewOpen] = useState(false)
  const [editing, setEditing] = useState<User | null>(null)
  const [deleting, setDeleting] = useState<User | null>(null)
  const [tokensFor, setTokensFor] = useState<User | null>(null)

  const users = data ?? []

  return (
    <div className="space-y-6 max-w-7xl">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-white">Users & Tokens</h1>
          <p className="text-sm text-gray-500 mt-1">
            Panel users and API tokens for automation
          </p>
        </div>
        <button
          type="button"
          onClick={() => setNewOpen(true)}
          className="inline-flex items-center gap-1.5 px-3 py-1.5 text-sm rounded bg-blue-600 hover:bg-blue-500 text-white transition"
        >
          <Plus size={16} />
          New User
        </button>
      </div>

      {users.length === 0 ? (
        <div className="rounded-lg border border-gray-800 bg-gray-900 p-12 text-center">
          <UsersIcon className="mx-auto text-gray-600 mb-3" size={32} />
          <div className="text-sm text-gray-400">No users</div>
        </div>
      ) : (
        <div className="rounded-lg border border-gray-800 bg-gray-900 overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="bg-gray-800/50 text-xs uppercase tracking-wider text-gray-500">
              <tr>
                <th className="px-4 py-2 text-left font-medium">Email</th>
                <th className="px-4 py-2 text-left font-medium">Role</th>
                <th className="px-4 py-2 text-left font-medium">Created</th>
                <th className="px-4 py-2 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-800">
              {users.map((u) => (
                <tr key={u.id} className="hover:bg-gray-800/30 transition">
                  <td className="px-4 py-2.5 text-white font-medium">
                    {u.email}
                  </td>
                  <td className="px-4 py-2.5">
                    <Badge variant={roleVariant(u.role)}>{u.role}</Badge>
                  </td>
                  <td className="px-4 py-2.5 text-gray-400 text-xs">
                    {formatDate(u.created_at)}
                  </td>
                  <td className="px-4 py-2.5 text-right">
                    <div className="inline-flex gap-1.5">
                      <button
                        type="button"
                        onClick={() => setTokensFor(u)}
                        className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
                      >
                        <Key size={12} />
                        Tokens
                      </button>
                      <button
                        type="button"
                        onClick={() => setEditing(u)}
                        className="px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
                      >
                        <Pencil size={12} />
                      </button>
                      <button
                        type="button"
                        onClick={() => setDeleting(u)}
                        className="px-2 py-1 text-xs rounded bg-red-600/20 hover:bg-red-600/30 text-red-400 border border-red-600/40 transition"
                      >
                        <Trash2 size={12} />
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <UserModal
        open={newOpen}
        edit={null}
        onClose={() => setNewOpen(false)}
        onSaved={() => {
          setNewOpen(false)
          mutate()
        }}
      />
      <UserModal
        open={!!editing}
        edit={editing}
        onClose={() => setEditing(null)}
        onSaved={() => {
          setEditing(null)
          mutate()
        }}
      />

      <ConfirmDelete
        open={!!deleting}
        title="Delete user"
        message={`Permanently delete user "${deleting?.email ?? ''}" and all their tokens?`}
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting) return
          await deleteUser(deleting.id)
          setDeleting(null)
          mutate()
        }}
      />

      <TokensModal
        user={tokensFor}
        onClose={() => setTokensFor(null)}
      />
    </div>
  )
}

function UserModal({
  open,
  edit,
  onClose,
  onSaved,
}: {
  open: boolean
  edit: User | null
  onClose: () => void
  onSaved: () => void
}) {
  const [form, setForm] = useState({
    email: '',
    password: '',
    role: 'viewer',
  })
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  useStateReset(JSON.stringify({ open, eid: edit?.id ?? null }), () => {
    if (!open) return
    if (edit) {
      setForm({ email: edit.email, password: '', role: edit.role })
    } else {
      setForm({ email: '', password: '', role: 'viewer' })
    }
    setErr(null)
  })

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr(null)
    try {
      if (edit) {
        const data: Partial<{ email: string; password: string; role: string }> = {
          email: form.email,
          role: form.role,
        }
        if (form.password) data.password = form.password
        await updateUser(edit.id, data)
      } else {
        await createUser(form)
      }
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
      title={edit ? 'Edit user' : 'New user'}
    >
      <form onSubmit={onSubmit} className="space-y-3">
        <FormField label="Email">
          <input
            type="email"
            required
            value={form.email}
            onChange={(e) => setForm({ ...form, email: e.target.value })}
            className={inputClass}
            placeholder="user@example.com"
          />
        </FormField>
        <FormField
          label={edit ? 'New password (leave blank to keep current)' : 'Password'}
        >
          <input
            type="password"
            required={!edit}
            value={form.password}
            onChange={(e) => setForm({ ...form, password: e.target.value })}
            className={inputClass}
            placeholder="••••••••"
          />
        </FormField>
        <FormField label="Role">
          <select
            value={form.role}
            onChange={(e) => setForm({ ...form, role: e.target.value })}
            className={inputClass}
          >
            {ROLES.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
        </FormField>
        <ErrorBox msg={err} />
        <ModalActions
          busy={busy}
          onCancel={onClose}
          submitLabel={edit ? 'Save' : 'Create'}
        />
      </form>
    </Modal>
  )
}

function TokensModal({
  user,
  onClose,
}: {
  user: User | null
  onClose: () => void
}) {
  const { data, mutate } = useSWR(
    user ? ['user-tokens', user.id] : null,
    () => listTokens(user!.id),
    { refreshInterval: 0 },
  )
  const [newOpen, setNewOpen] = useState(false)
  const [createdToken, setCreatedToken] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)
  const [deleting, setDeleting] = useState<APIToken | null>(null)

  const tokens = data ?? []

  return (
    <>
      <Modal
        open={!!user}
        onClose={() => {
          if (!createdToken) onClose()
        }}
        title={`Tokens for ${user?.email ?? ''}`}
        size="lg"
      >
        <div className="space-y-4">
          {createdToken && (
            <div className="rounded-md bg-yellow-500/10 border border-yellow-500/40 p-3">
              <div className="flex gap-2 text-sm text-yellow-300 mb-2">
                <AlertTriangle size={16} className="flex-shrink-0" />
                <span className="font-medium">
                  Copy this token now — it won't be shown again.
                </span>
              </div>
              <div className="flex items-center gap-2">
                <code className="flex-1 bg-gray-950 border border-gray-700 rounded px-2 py-1.5 text-xs text-gray-200 font-mono break-all">
                  {createdToken}
                </code>
                <button
                  type="button"
                  onClick={async () => {
                    if (await copyToClipboard(createdToken)) {
                      setCopied(true)
                      setTimeout(() => setCopied(false), 1500)
                    }
                  }}
                  className="inline-flex items-center gap-1 px-2 py-1.5 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
                >
                  {copied ? (
                    <>
                      <Check size={12} className="text-green-400" />
                      Copied
                    </>
                  ) : (
                    <>
                      <Copy size={12} />
                      Copy
                    </>
                  )}
                </button>
              </div>
              <div className="mt-3 flex justify-end">
                <button
                  type="button"
                  onClick={() => setCreatedToken(null)}
                  className="px-2 py-1 text-xs rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
                >
                  Done
                </button>
              </div>
            </div>
          )}

          <div className="flex items-center justify-between">
            <h3 className="text-sm font-medium text-gray-200">Active tokens</h3>
            <button
              type="button"
              onClick={() => setNewOpen(true)}
              className="inline-flex items-center gap-1 px-2 py-1 text-xs rounded bg-blue-600 hover:bg-blue-500 text-white transition"
            >
              <Plus size={14} />
              Create token
            </button>
          </div>

          {tokens.length === 0 ? (
            <div className="text-sm text-gray-500 py-4 text-center">
              No tokens for this user
            </div>
          ) : (
            <div className="rounded border border-gray-800 overflow-hidden">
              <table className="w-full text-sm">
                <thead className="bg-gray-800/40 text-xs uppercase text-gray-500">
                  <tr>
                    <th className="px-3 py-2 text-left font-medium">Name</th>
                    <th className="px-3 py-2 text-left font-medium">Scopes</th>
                    <th className="px-3 py-2 text-left font-medium">Created</th>
                    <th className="px-3 py-2 text-right font-medium">Actions</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-gray-800">
                  {tokens.map((t) => (
                    <tr key={t.id} className="hover:bg-gray-800/30 transition">
                      <td className="px-3 py-2 text-white">{t.name}</td>
                      <td className="px-3 py-2">
                        <div className="flex flex-wrap gap-1">
                          {(t.scopes || []).map((s) => (
                            <Badge key={s} variant="blue">
                              {s}
                            </Badge>
                          ))}
                        </div>
                      </td>
                      <td className="px-3 py-2 text-gray-400 text-xs">
                        {formatDate(t.created_at)}
                      </td>
                      <td className="px-3 py-2 text-right">
                        <button
                          type="button"
                          onClick={() => setDeleting(t)}
                          className="px-2 py-1 text-xs rounded bg-red-600/20 hover:bg-red-600/30 text-red-400 border border-red-600/40 transition"
                        >
                          <Trash2 size={12} />
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          <div className="flex justify-end pt-2 border-t border-gray-800">
            <button
              type="button"
              onClick={onClose}
              className="px-3 py-1.5 text-sm rounded bg-gray-800 hover:bg-gray-700 text-gray-300 border border-gray-700 transition"
            >
              Close
            </button>
          </div>
        </div>
      </Modal>

      {user && (
        <CreateTokenModal
          open={newOpen}
          userId={user.id}
          onClose={() => setNewOpen(false)}
          onCreated={(tok) => {
            setNewOpen(false)
            setCreatedToken(tok)
            mutate()
          }}
        />
      )}

      <ConfirmDelete
        open={!!deleting}
        title="Delete API token"
        message={`Revoke token "${deleting?.name ?? ''}"? Apps using this token will lose access immediately.`}
        confirmLabel="Revoke"
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting || !user) return
          await deleteToken(user.id, deleting.id)
          setDeleting(null)
          mutate()
        }}
      />
    </>
  )
}

function CreateTokenModal({
  open,
  userId,
  onClose,
  onCreated,
}: {
  open: boolean
  userId: string
  onClose: () => void
  onCreated: (tok: string) => void
}) {
  const [name, setName] = useState('')
  const [scopes, setScopes] = useState<string[]>(['read'])
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  useStateReset(JSON.stringify(open), () => {
    if (open) {
      setName('')
      setScopes(['read'])
      setErr(null)
    }
  })

  const toggle = (s: string) => {
    setScopes((cur) =>
      cur.includes(s) ? cur.filter((x) => x !== s) : [...cur, s],
    )
  }

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr(null)
    try {
      const r = await createToken(userId, { name, scopes })
      onCreated(r.token)
    } catch (e: any) {
      setErr(e?.message ?? 'Failed to create token')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={() => !busy && onClose()}
      title="Create API token"
    >
      <form onSubmit={onSubmit} className="space-y-3">
        <FormField label="Name">
          <input
            required
            value={name}
            onChange={(e) => setName(e.target.value)}
            className={inputClass}
            placeholder="CI publishing"
          />
        </FormField>
        <FormField label="Scopes">
          <div className="flex flex-wrap gap-2">
            {ALL_SCOPES.map((s) => (
              <label
                key={s}
                className={
                  'flex items-center gap-1.5 px-2.5 py-1 rounded border cursor-pointer text-xs transition ' +
                  (scopes.includes(s)
                    ? 'bg-blue-600/20 border-blue-500/50 text-blue-300'
                    : 'bg-gray-800 border-gray-700 text-gray-400 hover:border-gray-600')
                }
              >
                <input
                  type="checkbox"
                  className="hidden"
                  checked={scopes.includes(s)}
                  onChange={() => toggle(s)}
                />
                {s}
              </label>
            ))}
          </div>
        </FormField>
        <ErrorBox msg={err} />
        <ModalActions
          busy={busy}
          onCancel={onClose}
          submitLabel="Create"
        />
      </form>
    </Modal>
  )
}
