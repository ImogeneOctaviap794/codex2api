import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Loader2, CheckCircle2, AlertTriangle, ShieldCheck, Lock, Users } from 'lucide-react'
import Modal from './Modal'
import { Button } from '@/components/ui/button'
import { api } from '../api'
import type { DuplicateGroup, ToastType } from '../types'
import { getErrorMessage } from '../utils/error'

interface DedupeModalProps {
  show: boolean
  onClose: () => void
  onDone: () => void
  showToast: (msg: string, type?: ToastType) => void
}

/**
 * 账号去重预览 + 执行 Modal。
 *
 * UX 流程：
 *   1. 打开 Modal → loading → GET /accounts/duplicates
 *   2. 展示所有重复组：winner 绿色勾 / losers 红色 + checkbox（默认全选要删除）
 *   3. 底部显示「将删除 N 条」
 *   4. 点「执行软删」→ 二次 confirm → POST /accounts/dedupe
 *   5. 成功后 toast + onDone() 刷新父列表
 */
export default function DedupeModal({ show, onClose, onDone, showToast }: DedupeModalProps) {
  const { t } = useTranslation()
  const [loading, setLoading] = useState(false)
  const [groups, setGroups] = useState<DuplicateGroup[]>([])
  const [scannedCount, setScannedCount] = useState(0)
  const [winnerRule, setWinnerRule] = useState('')
  // 待删除 loser id 集合（用户勾选）
  const [toDelete, setToDelete] = useState<Set<number>>(new Set())
  const [executing, setExecuting] = useState(false)
  const [confirming, setConfirming] = useState(false)

  const loadDuplicates = async () => {
    setLoading(true)
    try {
      const res = await api.listDuplicateAccounts()
      setGroups(res.groups || [])
      setScannedCount(res.scanned_accounts || 0)
      setWinnerRule(res.winner_rule || '')
      // 默认全选所有 loser
      const allLosers = new Set<number>()
      for (const g of res.groups || []) {
        for (const l of g.losers) allLosers.add(l.id)
      }
      setToDelete(allLosers)
    } catch (err) {
      showToast(t('accounts.dedupeLoadFailed', { error: getErrorMessage(err) }), 'error')
      onClose()
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    if (show) {
      void loadDuplicates()
    } else {
      // 关闭时重置
      setGroups([])
      setToDelete(new Set())
      setConfirming(false)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [show])

  const toggleLoser = (id: number) => {
    setToDelete(prev => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const toggleGroup = (group: DuplicateGroup, allSelected: boolean) => {
    setToDelete(prev => {
      const next = new Set(prev)
      for (const l of group.losers) {
        if (allSelected) next.delete(l.id)
        else next.add(l.id)
      }
      return next
    })
  }

  const toggleAll = (checked: boolean) => {
    const next = new Set<number>()
    if (checked) {
      for (const g of groups) for (const l of g.losers) next.add(l.id)
    }
    setToDelete(next)
  }

  const totalLosers = useMemo(() => groups.reduce((sum, g) => sum + g.losers.length, 0), [groups])
  const selectedCount = toDelete.size

  const handleExecute = async () => {
    if (selectedCount === 0) return
    if (!confirming) {
      setConfirming(true)
      return
    }
    setExecuting(true)
    try {
      const ids = Array.from(toDelete)
      const res = await api.dedupeAccounts(ids)
      showToast(t('accounts.dedupeSuccess', { count: res.deleted }))
      onDone()
      onClose()
    } catch (err) {
      showToast(t('accounts.dedupeFailed', { error: getErrorMessage(err) }), 'error')
    } finally {
      setExecuting(false)
      setConfirming(false)
    }
  }

  return (
    <Modal
      show={show}
      onClose={onClose}
      title={t('accounts.dedupeTitle')}
      contentClassName="sm:max-w-[900px]"
      footer={
        <div className="flex w-full items-center justify-between gap-3">
          <div className="text-sm text-muted-foreground">
            {loading
              ? t('accounts.dedupeScanning')
              : totalLosers === 0
                ? t('accounts.dedupeNoDuplicates')
                : t('accounts.dedupeFooterSummary', {
                    groups: groups.length,
                    losers: totalLosers,
                    selected: selectedCount,
                    scanned: scannedCount,
                  })}
          </div>
          <div className="flex items-center gap-2">
            <Button variant="outline" onClick={onClose} disabled={executing}>
              {t('common.cancel')}
            </Button>
            {totalLosers > 0 && (
              <Button
                variant="destructive"
                onClick={() => void handleExecute()}
                disabled={selectedCount === 0 || executing}
              >
                {executing ? (
                  <>
                    <Loader2 className="size-3 animate-spin" />
                    {t('accounts.dedupeExecuting')}
                  </>
                ) : confirming ? (
                  t('accounts.dedupeConfirmAgain', { count: selectedCount })
                ) : (
                  t('accounts.dedupeExecute', { count: selectedCount })
                )}
              </Button>
            )}
          </div>
        </div>
      }
    >
      {loading ? (
        <div className="flex items-center justify-center py-12 text-muted-foreground">
          <Loader2 className="mr-2 size-4 animate-spin" />
          {t('accounts.dedupeScanning')}
        </div>
      ) : groups.length === 0 ? (
        <div className="flex flex-col items-center justify-center gap-3 py-12 text-muted-foreground">
          <CheckCircle2 className="size-12 text-emerald-500" />
          <div className="text-base font-medium text-foreground">{t('accounts.dedupeNoDuplicates')}</div>
          <div className="text-xs">{t('accounts.dedupeNoDuplicatesDesc', { count: scannedCount })}</div>
        </div>
      ) : (
        <div className="space-y-4">
          {/* 顶部说明 + 全选 */}
          <div className="flex items-start justify-between gap-4 rounded-md border border-amber-500/30 bg-amber-500/5 p-3 text-xs">
            <div className="flex items-start gap-2">
              <AlertTriangle className="mt-0.5 size-4 shrink-0 text-amber-600" />
              <div className="space-y-1">
                <div className="font-semibold text-foreground">{t('accounts.dedupeNotice')}</div>
                <div className="text-muted-foreground">{t('accounts.dedupeNoticeDetail')}</div>
                <div className="font-mono text-[10px] text-muted-foreground">{winnerRule}</div>
              </div>
            </div>
            <label className="flex shrink-0 items-center gap-1.5 text-foreground">
              <input
                type="checkbox"
                className="size-4 accent-[hsl(var(--primary))]"
                checked={selectedCount === totalLosers && totalLosers > 0}
                onChange={e => toggleAll(e.target.checked)}
              />
              {t('accounts.dedupeSelectAll')}
            </label>
          </div>

          {/* 分组列表 */}
          <div className="space-y-3">
            {groups.map(group => {
              const groupLoserIds = group.losers.map(l => l.id)
              const allSelected = groupLoserIds.every(id => toDelete.has(id))
              const someSelected = groupLoserIds.some(id => toDelete.has(id))

              return (
                <div key={`${group.email}-${group.platform}`} className="rounded-md border border-border bg-background">
                  {/* 分组头 */}
                  <div className="flex items-center justify-between gap-3 border-b border-border bg-muted/30 px-3 py-2">
                    <div className="flex min-w-0 items-center gap-2">
                      <Users className="size-4 shrink-0 text-muted-foreground" />
                      <span className="truncate font-mono text-sm font-semibold text-foreground">{group.email}</span>
                      <span className="shrink-0 rounded-sm bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
                        {group.platform}
                      </span>
                      <span className="shrink-0 text-xs text-muted-foreground">
                        {t('accounts.dedupeGroupCount', { count: group.total_in_group })}
                      </span>
                    </div>
                    <label className="flex shrink-0 items-center gap-1.5 text-xs text-muted-foreground">
                      <input
                        type="checkbox"
                        className="size-4 accent-[hsl(var(--primary))]"
                        checked={allSelected}
                        ref={el => { if (el) el.indeterminate = someSelected && !allSelected }}
                        onChange={() => toggleGroup(group, allSelected)}
                      />
                      {t('accounts.dedupeToggleGroup')}
                    </label>
                  </div>

                  {/* Winner 行 */}
                  <AccountRowCard account={group.winner} role="winner" />

                  {/* Loser 行 */}
                  {group.losers.map(loser => (
                    <AccountRowCard
                      key={loser.id}
                      account={loser}
                      role="loser"
                      checked={toDelete.has(loser.id)}
                      onToggle={() => toggleLoser(loser.id)}
                    />
                  ))}
                </div>
              )
            })}
          </div>
        </div>
      )}
    </Modal>
  )
}

interface AccountRowCardProps {
  account: {
    id: number
    name: string
    email: string
    platform: string
    status: string
    plan_type: string
    has_rt: boolean
    has_at: boolean
    locked: boolean
    created_at: string
    last_used_at?: string
    score: number
  }
  role: 'winner' | 'loser'
  checked?: boolean
  onToggle?: () => void
}

function AccountRowCard({ account, role, checked, onToggle }: AccountRowCardProps) {
  const { t } = useTranslation()
  const isWinner = role === 'winner'

  return (
    <div
      className={
        'flex items-center gap-3 px-3 py-2 text-sm border-b border-border last:border-b-0 ' +
        (isWinner
          ? 'bg-emerald-500/5'
          : checked
            ? 'bg-destructive/5'
            : 'bg-background')
      }
    >
      {/* 选择框（只有 loser 有） */}
      {isWinner ? (
        <div className="flex size-5 shrink-0 items-center justify-center">
          <ShieldCheck className="size-4 text-emerald-600 dark:text-emerald-400" />
        </div>
      ) : (
        <input
          type="checkbox"
          className="size-4 shrink-0 accent-[hsl(var(--destructive))]"
          checked={checked}
          onChange={onToggle}
        />
      )}

      {/* ID */}
      <div className="w-14 shrink-0 font-mono text-xs text-muted-foreground">#{account.id}</div>

      {/* 主体信息 */}
      <div className="min-w-0 flex-1 space-y-0.5">
        <div className="flex items-center gap-1.5">
          <span className={'truncate text-sm font-medium ' + (isWinner ? 'text-emerald-700 dark:text-emerald-400' : 'text-foreground')}>
            {account.name || account.email}
          </span>
          {account.locked && <Lock className="size-3 text-blue-600" />}
        </div>
        <div className="flex flex-wrap items-center gap-1.5 text-[11px] text-muted-foreground">
          <span className={'rounded px-1 py-0.5 ' + statusBadgeClass(account.status)}>{account.status}</span>
          {account.plan_type && (
            <span className="rounded bg-muted px-1 py-0.5">{account.plan_type}</span>
          )}
          {account.has_rt ? (
            <span className="rounded bg-emerald-500/10 px-1 py-0.5 text-emerald-700 dark:text-emerald-400">RT</span>
          ) : account.has_at ? (
            <span className="rounded bg-amber-500/10 px-1 py-0.5 text-amber-700 dark:text-amber-400">AT-only</span>
          ) : null}
          <span>{t('accounts.dedupeScore', { score: account.score })}</span>
          <span>·</span>
          <span>{new Date(account.created_at).toLocaleDateString()}</span>
        </div>
      </div>

      {/* 角色标签 */}
      <div className="shrink-0 text-xs font-semibold">
        {isWinner ? (
          <span className="text-emerald-600 dark:text-emerald-400">{t('accounts.dedupeWinnerLabel')}</span>
        ) : (
          <span className={checked ? 'text-destructive' : 'text-muted-foreground'}>
            {checked ? t('accounts.dedupeLoserLabel') : t('accounts.dedupeSkipLabel')}
          </span>
        )}
      </div>
    </div>
  )
}

function statusBadgeClass(status: string): string {
  switch (status) {
    case 'active':
    case 'normal':
      return 'bg-emerald-500/10 text-emerald-700 dark:text-emerald-400'
    case 'rate_limited':
    case 'usage_exhausted':
      return 'bg-amber-500/10 text-amber-700 dark:text-amber-400'
    case 'banned':
    case 'error':
      return 'bg-destructive/10 text-destructive'
    default:
      return 'bg-muted text-muted-foreground'
  }
}
