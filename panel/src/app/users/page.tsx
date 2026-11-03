'use client'

import { useState } from 'react'
import useSWR from 'swr'
import { Plus, Trash2, Pencil, Key, Users as UsersIcon, AlertTriangle, ShieldCheck, Wrench, Eye } from 'lucide-react'
import { Layout } from '@/components/Layout'
import { Modal } from '@/components/Modal'
import { Badge, BadgeVariant } from '@/components/Badge'
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
  DropdownMenu,
  EmptyState,
  TableSkeleton,
  CopyField,
  useToast,
} from '@/components/ui'
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
import { formatDate } from '@/lib/utils'

const ROLES = ['admin', 'operator', 'viewer']
const ALL_SCOPES = ['read', 'write', 'stream']

// One look per role so they're told apart at a glance; the role text is
// always shown too (color alone isn't a status).
function roleVariant(r: string): BadgeVariant {
  if (r === 'admin') return 'purple'
  if (r === 'operator') return 'blue'
  return 'gray'
}

function RoleIcon({ role }: { role: string }) {
  if (role === 'admin') return <ShieldCheck size={12} />
  if (role === 'operator') return <Wrench size={12} />
  return <Eye size={12} />
}

function RoleBadge({ role }: { role: string }) {
  return (
    <Badge variant={roleVariant(role)}>
      <RoleIcon role={role} />
      {role}
    </Badge>
  )
}

const ROLE_HINTS: Record<string, string> = {
  admin: 'Full access, including users and server settings.',
  operator: 'Manages applications, streams and sources.',
  viewer: 'Read-only access to the panel.',
}

const SCOPE_HINTS: Record<string, string> = {
  read: 'Read API resources',
  write: 'Create, change and delete resources',
  stream: 'Publish and play streams',
}

export default function UsersPage() {
  return (
    <Layout>
      <UsersContent />
    </Layout>
  )
}

function UsersContent() {
  const { data, mutate, isLoading } = useSWR('users', listUsers, { refreshInterval: 30000 })
  const toast = useToast()
  const [newOpen, setNewOpen] = useState(false)
  const [editing, setEditing] = useState<User | null>(null)
  const [deleting, setDeleting] = useState<User | null>(null)
  const [tokensFor, setTokensFor] = useState<User | null>(null)

  const users = data ?? []

  return (
    <>
      <PageHeader
        title="Users & Tokens"
        description="Panel users and API tokens for automation."
        actions={
          <Button variant="primary" icon={<Plus size={15} />} onClick={() => setNewOpen(true)}>
            New user
          </Button>
        }
      />

      {isLoading ? (
        <TableSkeleton rows={3} cols={4} />
      ) : users.length === 0 ? (
        <TableCard>
          <EmptyState
            icon={<UsersIcon size={20} />}
            title="No users"
            description="Create a user to give someone access to the panel or the API."
            action={
              <Button variant="primary" icon={<Plus size={15} />} onClick={() => setNewOpen(true)}>
                New user
              </Button>
            }
          />
        </TableCard>
      ) : (
        <TableCard>
          <Table>
            <THead>
              <tr>
                <TH>Email</TH>
                <TH>Role</TH>
                <TH>Created</TH>
                <TH align="right">
                  <span className="sr-only">Actions</span>
                </TH>
              </tr>
            </THead>
            <TBody>
              {users.map((u) => (
                <TR key={u.id}>
                  <TD>
                    <span className="font-medium text-fg">{u.email}</span>
                  </TD>
                  <TD>
                    <RoleBadge role={u.role} />
                  </TD>
                  <TD muted className="whitespace-nowrap">
                    {formatDate(u.created_at)}
                  </TD>
                  <TD align="right">
                    <RowActions>
                      <Button size="xs" variant="secondary" icon={<Key size={13} />} onClick={() => setTokensFor(u)}>
                        API tokens
                      </Button>
                      <IconButton label={`Edit ${u.email}`} size="xs" onClick={() => setEditing(u)}>
                        <Pencil size={14} />
                      </IconButton>
                      <DropdownMenu
                        label={`More actions for ${u.email}`}
                        items={[
                          { label: 'Edit user', icon: <Pencil size={14} />, onClick: () => setEditing(u) },
                          { label: 'Manage API tokens', icon: <Key size={14} />, onClick: () => setTokensFor(u) },
                          {
                            label: 'Delete user',
                            icon: <Trash2 size={14} />,
                            danger: true,
                            separator: true,
                            onClick: () => setDeleting(u),
                          },
                        ]}
                      />
                    </RowActions>
                  </TD>
                </TR>
              ))}
            </TBody>
          </Table>
        </TableCard>
      )}

      <UserModal
        open={newOpen}
        edit={null}
        onClose={() => setNewOpen(false)}
        onSaved={() => {
          setNewOpen(false)
          toast.success('User created')
          mutate()
        }}
      />
      <UserModal
        open={!!editing}
        edit={editing}
        onClose={() => setEditing(null)}
        onSaved={() => {
          setEditing(null)
          toast.success('User saved')
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
          toast.success(`Deleted ${deleting.email}`)
          setDeleting(null)
          mutate()
        }}
      />

      <TokensModal user={tokensFor} onClose={() => setTokensFor(null)} />
    </>
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
      description={edit ? edit.email : 'Panel login and API token owner.'}
    >
      <form onSubmit={onSubmit} className="space-y-4">
        <FormSection title="Sign-in">
          <FormField label="Email" required>
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
            label={edit ? 'New password' : 'Password'}
            required={!edit}
            hint={edit ? 'Leave blank to keep the current password.' : undefined}
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
        </FormSection>
        <FormSection title="Permissions">
          <FormField label="Role" hint={ROLE_HINTS[form.role]}>
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
        </FormSection>
        <ErrorBox msg={err} />
        <ModalActions busy={busy} onCancel={onClose} submitLabel={edit ? 'Save' : 'Create'} />
      </form>
    </Modal>
  )
}

function TokensModal({ user, onClose }: { user: User | null; onClose: () => void }) {
  const { data, mutate, isLoading } = useSWR(user ? ['user-tokens', user.id] : null, () => listTokens(user!.id), {
    refreshInterval: 0,
  })
  const toast = useToast()
  const [newOpen, setNewOpen] = useState(false)
  const [createdToken, setCreatedToken] = useState<string | null>(null)
  const [deleting, setDeleting] = useState<APIToken | null>(null)

  const tokens = data ?? []

  return (
    <>
      <Modal
        open={!!user}
        onClose={() => {
          if (!createdToken) onClose()
        }}
        title="API tokens"
        description={user?.email}
        size="lg"
      >
        <div className="space-y-4">
          {createdToken && (
            <div className="space-y-3 rounded-md border border-warning/40 bg-warning/10 p-3">
              <div className="flex gap-2 text-sm text-warning">
                <AlertTriangle size={16} className="mt-0.5 shrink-0" />
                <span className="font-medium">Copy this token now. It won&apos;t be shown again.</span>
              </div>
              <CopyField value={createdToken} />
              <div className="flex justify-end">
                <Button size="xs" variant="secondary" onClick={() => setCreatedToken(null)}>
                  Done
                </Button>
              </div>
            </div>
          )}

          <div className="flex flex-wrap items-center justify-between gap-2">
            <h3 className="text-[15px] font-semibold text-fg">Active tokens</h3>
            <Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={() => setNewOpen(true)}>
              Create token
            </Button>
          </div>

          {isLoading ? (
            <TableSkeleton rows={2} cols={3} />
          ) : tokens.length === 0 ? (
            <div className="rounded-lg border border-border">
              <EmptyState
                compact
                icon={<Key size={20} />}
                title="No tokens"
                description="Create a token to call the API from scripts or CI."
              />
            </div>
          ) : (
            <TableCard>
              <Table>
                <THead>
                  <tr>
                    <TH>Name</TH>
                    <TH>Scopes</TH>
                    <TH>Created</TH>
                    <TH align="right">
                      <span className="sr-only">Actions</span>
                    </TH>
                  </tr>
                </THead>
                <TBody>
                  {tokens.map((t) => (
                    <TR key={t.id}>
                      <TD>
                        <span className="font-medium text-fg">{t.name}</span>
                      </TD>
                      <TD>
                        <div className="flex flex-wrap gap-1">
                          {(t.scopes || []).map((s) => (
                            <Badge key={s} variant="blue" mono title={SCOPE_HINTS[s]}>
                              {s}
                            </Badge>
                          ))}
                        </div>
                      </TD>
                      <TD muted className="whitespace-nowrap">
                        {formatDate(t.created_at)}
                      </TD>
                      <TD align="right">
                        <RowActions>
                          <Button
                            size="xs"
                            variant="danger-outline"
                            icon={<Trash2 size={13} />}
                            onClick={() => setDeleting(t)}
                          >
                            Revoke
                          </Button>
                        </RowActions>
                      </TD>
                    </TR>
                  ))}
                </TBody>
              </Table>
            </TableCard>
          )}

          <div className="flex justify-end border-t border-border pt-4">
            <Button size="md" variant="secondary" onClick={onClose}>
              Close
            </Button>
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
            toast.success('Token created')
            mutate()
          }}
        />
      )}

      <ConfirmDelete
        open={!!deleting}
        title="Revoke API token"
        message={`Revoke token "${deleting?.name ?? ''}"? Apps using this token will lose access immediately.`}
        confirmLabel="Revoke"
        onCancel={() => setDeleting(null)}
        onConfirm={async () => {
          if (!deleting || !user) return
          await deleteToken(user.id, deleting.id)
          toast.success(`Revoked token "${deleting.name}"`)
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
    setScopes((cur) => (cur.includes(s) ? cur.filter((x) => x !== s) : [...cur, s]))
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
      description="The token is shown once, right after it's created."
    >
      <form onSubmit={onSubmit} className="space-y-4">
        <FormField label="Name" required hint="Something that says where it's used.">
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
            {ALL_SCOPES.map((s) => {
              const on = scopes.includes(s)
              return (
                <label
                  key={s}
                  title={SCOPE_HINTS[s]}
                  className={
                    'flex cursor-pointer items-center gap-1.5 rounded-md border px-2.5 py-1 font-mono text-xs transition-colors focus-within:ring-2 focus-within:ring-primary/40 ' +
                    (on
                      ? 'border-primary/50 bg-primary/15 text-primary-fg'
                      : 'border-border bg-elevated text-fg-secondary hover:border-border-strong')
                  }
                >
                  <input type="checkbox" className="sr-only" checked={on} onChange={() => toggle(s)} />
                  {s}
                </label>
              )
            })}
          </div>
        </FormField>
        <ErrorBox msg={err} />
        <ModalActions busy={busy} onCancel={onClose} submitLabel="Create" />
      </form>
    </Modal>
  )
}
