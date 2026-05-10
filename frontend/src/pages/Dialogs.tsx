import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Activity,
  AlertTriangle,
  CheckCircle2,
  ChevronLeft,
  ChevronRight,
  Database,
  Eye,
  HardDrive,
  Inbox,
  RefreshCw,
} from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api, type DialogLogDetail, type DialogLogSummary, type DialogStatsResponse } from '../api'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import { usePolling } from '../hooks/usePolling'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Select, type SelectOption } from '@/components/ui/select'
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog'

const ENDPOINT_OPTIONS: SelectOption[] = [
  { label: '全部 endpoint', value: '' },
  { label: '/v1/chat/completions', value: '/v1/chat/completions' },
  { label: '/v1/responses', value: '/v1/responses' },
  { label: '/v1/responses/compact', value: '/v1/responses/compact' },
]

const MODEL_OPTIONS: SelectOption[] = [
  { label: '全部模型', value: '' },
  { label: 'gpt-5', value: 'gpt-5' },
  { label: 'gpt-5-codex', value: 'gpt-5-codex' },
  { label: 'gpt-5.4', value: 'gpt-5.4' },
  { label: 'gpt-5.5', value: 'gpt-5.5' },
]

const PAGE_SIZE_OPTIONS: SelectOption[] = [
  { label: '20 条/页', value: '20' },
  { label: '50 条/页', value: '50' },
  { label: '100 条/页', value: '100' },
]

function formatNumber(n: number | undefined): string {
  if (n == null) return '0'
  return n.toLocaleString()
}

function formatTime(ts?: string): string {
  if (!ts) return '--'
  try {
    return new Date(ts).toLocaleString()
  } catch {
    return ts
  }
}

function formatBytes(n?: number): string {
  if (!n || n <= 0) return '0 B'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(2)} MB`
}

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(1)}s`
}

interface MetricCardProps {
  label: string
  value: string
  hint?: string
  icon: React.ReactNode
  tone?: 'normal' | 'warning' | 'danger' | 'info'
}

function MetricCard({ label, value, hint, icon, tone = 'normal' }: MetricCardProps) {
  const toneColor = {
    normal: 'text-foreground',
    info: 'text-blue-600 dark:text-blue-400',
    warning: 'text-amber-600 dark:text-amber-400',
    danger: 'text-rose-600 dark:text-rose-400',
  }[tone]
  return (
    <Card>
      <CardContent className="p-5">
        <div className="flex items-center justify-between">
          <span className="text-sm text-muted-foreground">{label}</span>
          <span className={toneColor}>{icon}</span>
        </div>
        <div className={`mt-3 text-3xl font-semibold tabular-nums ${toneColor}`}>{value}</div>
        {hint ? <div className="mt-1.5 text-xs text-muted-foreground">{hint}</div> : null}
      </CardContent>
    </Card>
  )
}

// =====================================================================
// 详情抽屉：展示完整请求 / 响应 / 推理内容
// =====================================================================

interface DetailDialogProps {
  id: number | null
  onClose: () => void
}

function DetailDialog({ id, onClose }: DetailDialogProps) {
  const [detail, setDetail] = useState<DialogLogDetail | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [tab, setTab] = useState<'request' | 'response' | 'reasoning' | 'tools'>('request')

  useEffect(() => {
    if (id == null) {
      setDetail(null)
      return
    }
    setLoading(true)
    setError(null)
    setTab('request')
    api
      .getDialogLogDetail(id)
      .then((d) => setDetail(d))
      .catch((e) => setError(e instanceof Error ? e.message : '加载失败'))
      .finally(() => setLoading(false))
  }, [id])

  const open = id != null
  const requestText = useMemo(
    () => (detail?.request_body ? JSON.stringify(detail.request_body, null, 2) : ''),
    [detail],
  )
  const responseText = useMemo(
    () => (detail?.response_body ? JSON.stringify(detail.response_body, null, 2) : ''),
    [detail],
  )
  const toolCallsText = useMemo(
    () => (detail?.tool_calls ? JSON.stringify(detail.tool_calls, null, 2) : ''),
    [detail],
  )

  const TabBtn = ({
    name,
    label,
    count,
  }: {
    name: typeof tab
    label: string
    count?: number
  }) => (
    <button
      type="button"
      onClick={() => setTab(name)}
      className={`px-3 py-1.5 text-sm rounded-md transition-colors ${
        tab === name
          ? 'bg-primary text-primary-foreground'
          : 'bg-muted text-muted-foreground hover:bg-accent'
      }`}
    >
      {label}
      {count != null && count > 0 ? <span className="ml-1.5 opacity-80">({count})</span> : null}
    </button>
  )

  return (
    <Dialog open={open} onOpenChange={(v) => !v && onClose()}>
      <DialogContent className="max-w-[min(1100px,95vw)] max-h-[90vh] overflow-hidden flex flex-col">
        <DialogHeader>
          <DialogTitle>对话详情 #{id}</DialogTitle>
        </DialogHeader>

        {loading ? (
          <div className="text-center py-12 text-muted-foreground">加载中...</div>
        ) : error ? (
          <div className="text-center py-12 text-rose-500">{error}</div>
        ) : !detail ? (
          <div className="text-center py-12 text-muted-foreground">未找到记录</div>
        ) : (
          <>
            {/* 元信息卡片 */}
            <div className="grid grid-cols-2 md:grid-cols-4 gap-3 text-sm">
              <div>
                <div className="text-muted-foreground">时间</div>
                <div className="font-mono mt-0.5">{formatTime(detail.ts)}</div>
              </div>
              <div>
                <div className="text-muted-foreground">Endpoint</div>
                <div className="font-mono mt-0.5 truncate">{detail.endpoint}</div>
              </div>
              <div>
                <div className="text-muted-foreground">模型</div>
                <div className="font-mono mt-0.5">
                  {detail.model || '--'}
                  {detail.base_model ? ` → ${detail.base_model}` : ''}
                </div>
              </div>
              <div>
                <div className="text-muted-foreground">Stream</div>
                <div className="mt-0.5">{detail.is_stream ? 'Yes' : 'No'}</div>
              </div>
              <div>
                <div className="text-muted-foreground">耗时</div>
                <div className="mt-0.5">{formatDuration(detail.duration_ms)}</div>
              </div>
              <div>
                <div className="text-muted-foreground">Tokens (in/out/reason)</div>
                <div className="mt-0.5">
                  {detail.prompt_tokens} / {detail.completion_tokens} / {detail.reasoning_tokens}
                </div>
              </div>
              <div>
                <div className="text-muted-foreground">账号 ID</div>
                <div className="font-mono mt-0.5">{detail.account_id || '--'}</div>
              </div>
              <div>
                <div className="text-muted-foreground">API key hash</div>
                <div className="font-mono mt-0.5 truncate text-xs">
                  {detail.api_key_hash || '--'}
                </div>
              </div>
            </div>

            {/* Tab 切换 */}
            <div className="flex gap-2 mt-4 flex-wrap">
              <TabBtn name="request" label="请求" />
              <TabBtn name="response" label="响应" />
              <TabBtn name="reasoning" label="推理" />
              {detail.has_tool_calls ? <TabBtn name="tools" label="Tool Calls" /> : null}
            </div>

            {/* Tab 内容 */}
            <div className="mt-3 flex-1 min-h-0 overflow-auto">
              {tab === 'request' ? (
                <pre className="text-xs font-mono bg-muted/50 p-4 rounded-lg overflow-auto whitespace-pre-wrap break-all max-h-[60vh]">
                  {requestText || '(空)'}
                </pre>
              ) : tab === 'response' ? (
                <pre className="text-xs font-mono bg-muted/50 p-4 rounded-lg overflow-auto whitespace-pre-wrap break-all max-h-[60vh]">
                  {responseText || '(空)'}
                </pre>
              ) : tab === 'reasoning' ? (
                <pre className="text-sm font-mono bg-muted/50 p-4 rounded-lg overflow-auto whitespace-pre-wrap break-words max-h-[60vh]">
                  {detail.reasoning_content || '(本次响应无 reasoning_content)'}
                </pre>
              ) : (
                <pre className="text-xs font-mono bg-muted/50 p-4 rounded-lg overflow-auto whitespace-pre-wrap break-all max-h-[60vh]">
                  {toolCallsText || '(空)'}
                </pre>
              )}
            </div>

            <div className="flex items-center justify-between text-xs text-muted-foreground border-t pt-3 mt-2">
              <div>
                请求体 {formatBytes(detail.request_size)} · 响应体 {formatBytes(detail.response_size)}
              </div>
              <div>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    const text = tab === 'request' ? requestText : tab === 'response' ? responseText : tab === 'reasoning' ? detail.reasoning_content || '' : toolCallsText
                    void navigator.clipboard.writeText(text)
                  }}
                >
                  复制当前 Tab
                </Button>
              </div>
            </div>
          </>
        )}
      </DialogContent>
    </Dialog>
  )
}

// =====================================================================
// 主页面
// =====================================================================

export default function Dialogs() {
  const { t } = useTranslation()
  const [toggling, setToggling] = useState(false)
  const [endpointFilter, setEndpointFilter] = useState('')
  const [modelFilter, setModelFilter] = useState('')
  const [pageSize, setPageSize] = useState(50)
  const [page, setPage] = useState(0) // 0-based
  const [detailId, setDetailId] = useState<number | null>(null)

  const loadStats = useCallback(() => api.getDialogStats(), [])
  const { data: stats, loading: statsLoading, error: statsError, reload: reloadStats, reloadSilently: reloadStatsSilently } =
    useDataLoader<DialogStatsResponse | null>({ initialData: null, load: loadStats })
  usePolling(() => void reloadStatsSilently(), 5_000)

  const [logs, setLogs] = useState<DialogLogSummary[]>([])
  const [logsTotal, setLogsTotal] = useState(0)
  const [logsLoading, setLogsLoading] = useState(false)
  const [logsError, setLogsError] = useState<string | null>(null)

  const loadLogs = useCallback(async () => {
    setLogsLoading(true)
    setLogsError(null)
    try {
      const res = await api.listDialogLogs({
        endpoint: endpointFilter,
        model: modelFilter,
        limit: pageSize,
        offset: page * pageSize,
      })
      setLogs(res.items)
      setLogsTotal(res.total)
    } catch (e) {
      setLogsError(e instanceof Error ? e.message : '加载失败')
    } finally {
      setLogsLoading(false)
    }
  }, [endpointFilter, modelFilter, pageSize, page])

  useEffect(() => {
    void loadLogs()
  }, [loadLogs])

  const handleToggle = async () => {
    if (!stats?.runtime) return
    setToggling(true)
    try {
      await api.toggleDialogCollection(!stats.runtime.enabled)
      await reloadStats()
    } catch (e) {
      console.error('Toggle failed', e)
    } finally {
      setToggling(false)
    }
  }

  const installed = stats?.installed ?? false
  const runtime = stats?.runtime
  const db = stats?.db
  const queueUsagePct = runtime ? Math.round((runtime.queue_len / runtime.queue_cap) * 100) : 0
  const failRate =
    runtime && runtime.submitted > 0
      ? ((runtime.failed / runtime.submitted) * 100).toFixed(2)
      : '0.00'

  const totalPages = Math.ceil(logsTotal / pageSize)

  return (
    <StateShell
      variant="page"
      loading={statsLoading}
      error={statsError}
      onRetry={() => void reloadStats()}
      loadingTitle={t('dialogs.loadingTitle', '加载对话采集数据')}
      loadingDescription={t('dialogs.loadingDesc', '同步采集运行时与落盘统计...')}
      errorTitle={t('dialogs.errorTitle', '加载失败')}
    >
      <>
        <PageHeader
          title={t('dialogs.title', '对话采集')}
          description={t(
            'dialogs.description',
            '原始对话数据落盘统计 + 抽样浏览。完全异步、有损（队列满即丢）、零客户端感知。',
          )}
          actions={
            <div className="flex items-center gap-3 max-sm:w-full max-sm:flex-col max-sm:items-stretch">
              <Button variant="outline" onClick={() => void reloadStats()}>
                <RefreshCw className="size-3.5" />
                {t('common.refresh', '刷新')}
              </Button>
              {installed && runtime ? (
                <Button
                  variant={runtime.enabled ? 'destructive' : 'default'}
                  onClick={handleToggle}
                  disabled={toggling}
                >
                  {runtime.enabled ? '关闭采集' : '开启采集'}
                </Button>
              ) : null}
            </div>
          }
        />

        {!installed ? (
          <Card>
            <CardContent className="p-6">
              <div className="flex items-start gap-3 text-amber-600 dark:text-amber-400">
                <AlertTriangle className="size-5 mt-0.5" />
                <div>
                  <div className="font-semibold">采集未启用</div>
                  <div className="mt-1 text-sm text-muted-foreground">
                    可能原因：(1) 启动时 ENV{' '}
                    <code className="bg-muted px-1.5 py-0.5 rounded">DIALOG_COLLECTION_ENABLED=false</code>；
                    (2) 当前数据库不是 PostgreSQL（仅 PG 支持）；(3) 启动时建表失败（查容器日志）。
                  </div>
                </div>
              </div>
            </CardContent>
          </Card>
        ) : (
          <>
            {/* === 状态 banner === */}
            <Card className="mb-6">
              <CardContent className="p-5">
                <div className="flex items-center justify-between flex-wrap gap-3">
                  <div className="flex items-center gap-3">
                    {runtime?.enabled ? (
                      <CheckCircle2 className="size-5 text-emerald-500" />
                    ) : (
                      <AlertTriangle className="size-5 text-amber-500" />
                    )}
                    <div>
                      <div className="font-semibold">
                        {runtime?.enabled ? '采集运行中' : '采集已暂停'}
                      </div>
                      <div className="text-sm text-muted-foreground">
                        {runtime?.enabled
                          ? '所有成功的 chat/responses/responses-compact 请求会异步落盘到 dialog_logs'
                          : '运行时开关已关闭。已落盘数据保留，新请求不再采集。'}
                      </div>
                    </div>
                  </div>
                  <div className="text-xs text-muted-foreground">每 5 秒自动刷新</div>
                </div>
              </CardContent>
            </Card>

            {/* === 运行时指标 === */}
            <h3 className="mb-3 text-lg font-semibold">运行时指标</h3>
            <div className="grid grid-cols-2 lg:grid-cols-4 gap-4 mb-6">
              <MetricCard
                label="已提交"
                value={formatNumber(runtime?.submitted)}
                hint="主链路触发的采集事件总数"
                icon={<Activity className="size-4" />}
                tone="info"
              />
              <MetricCard
                label="已落盘"
                value={formatNumber(runtime?.written)}
                hint="成功写入 PG 的记录数"
                icon={<CheckCircle2 className="size-4" />}
              />
              <MetricCard
                label="丢弃"
                value={formatNumber(runtime?.dropped)}
                hint="队列满直接丢（保护主链路）"
                icon={<Inbox className="size-4" />}
                tone={(runtime?.dropped ?? 0) > 0 ? 'warning' : 'normal'}
              />
              <MetricCard
                label="写入失败"
                value={`${formatNumber(runtime?.failed)} (${failRate}%)`}
                hint="DB 写入异常的批次数"
                icon={<AlertTriangle className="size-4" />}
                tone={(runtime?.failed ?? 0) > 0 ? 'warning' : 'normal'}
              />
            </div>

            {/* === 队列状态 === */}
            <Card className="mb-6">
              <CardContent className="p-5">
                <div className="flex items-center justify-between mb-2">
                  <span className="font-medium">采集队列</span>
                  <span className="text-sm text-muted-foreground tabular-nums">
                    {formatNumber(runtime?.queue_len)} / {formatNumber(runtime?.queue_cap)} ({queueUsagePct}%)
                  </span>
                </div>
                <div className="h-2 w-full bg-muted rounded-full overflow-hidden">
                  <div
                    className={`h-full transition-all ${
                      queueUsagePct >= 80
                        ? 'bg-rose-500'
                        : queueUsagePct >= 50
                          ? 'bg-amber-500'
                          : 'bg-emerald-500'
                    }`}
                    style={{ width: `${Math.max(queueUsagePct, 1)}%` }}
                  />
                </div>
              </CardContent>
            </Card>

            {/* === 数据库累计 === */}
            <h3 className="mb-3 text-lg font-semibold">数据库累计</h3>
            <div className="grid grid-cols-2 lg:grid-cols-4 gap-4 mb-6">
              <MetricCard
                label="总行数"
                value={formatNumber(db?.total_rows)}
                hint="dialog_logs 全部分区"
                icon={<Database className="size-4" />}
                tone="info"
              />
              <MetricCard
                label="今日"
                value={formatNumber(db?.today)}
                icon={<Activity className="size-4" />}
              />
              <MetricCard
                label="昨日"
                value={formatNumber(db?.yesterday)}
                icon={<Activity className="size-4" />}
              />
              <MetricCard
                label="表大小"
                value={db?.table_size ?? '--'}
                hint={`${db?.partitions?.length ?? 0} 个月分区`}
                icon={<HardDrive className="size-4" />}
              />
            </div>

            {/* === 抽样浏览 === */}
            <h3 className="mb-3 text-lg font-semibold">抽样浏览</h3>
            <Card className="mb-6">
              <CardContent className="p-5">
                {/* 筛选栏 */}
                <div className="flex flex-wrap items-center gap-3 mb-4">
                  <div className="min-w-[200px]">
                    <Select
                      value={endpointFilter}
                      onValueChange={(v) => {
                        setEndpointFilter(v)
                        setPage(0)
                      }}
                      options={ENDPOINT_OPTIONS}
                      compact
                    />
                  </div>
                  <div className="min-w-[150px]">
                    <Select
                      value={modelFilter}
                      onValueChange={(v) => {
                        setModelFilter(v)
                        setPage(0)
                      }}
                      options={MODEL_OPTIONS}
                      compact
                    />
                  </div>
                  <div className="min-w-[120px]">
                    <Select
                      value={String(pageSize)}
                      onValueChange={(v) => {
                        setPageSize(Number(v))
                        setPage(0)
                      }}
                      options={PAGE_SIZE_OPTIONS}
                      compact
                    />
                  </div>
                  <Button size="sm" variant="outline" onClick={() => void loadLogs()}>
                    <RefreshCw className="size-3.5" />
                    刷新列表
                  </Button>
                  <div className="ml-auto text-sm text-muted-foreground">
                    共 <span className="font-mono">{formatNumber(logsTotal)}</span> 条
                    {(endpointFilter || modelFilter) ? '（已筛选）' : '（估算）'}
                  </div>
                </div>

                {/* 表格 */}
                <div className="overflow-x-auto -mx-2">
                  <table className="w-full text-sm">
                    <thead>
                      <tr className="border-b text-left text-xs text-muted-foreground uppercase">
                        <th className="px-3 py-2 font-medium">ID</th>
                        <th className="px-3 py-2 font-medium">时间</th>
                        <th className="px-3 py-2 font-medium">Endpoint</th>
                        <th className="px-3 py-2 font-medium">模型</th>
                        <th className="px-3 py-2 font-medium text-center">Stream</th>
                        <th className="px-3 py-2 font-medium text-right">Tokens</th>
                        <th className="px-3 py-2 font-medium text-right">耗时</th>
                        <th className="px-3 py-2 font-medium text-right">体积</th>
                        <th className="px-3 py-2 font-medium text-center">推理</th>
                        <th className="px-3 py-2 font-medium text-center">操作</th>
                      </tr>
                    </thead>
                    <tbody>
                      {logsLoading ? (
                        <tr>
                          <td colSpan={10} className="text-center py-8 text-muted-foreground">
                            加载中...
                          </td>
                        </tr>
                      ) : logsError ? (
                        <tr>
                          <td colSpan={10} className="text-center py-8 text-rose-500">
                            {logsError}
                          </td>
                        </tr>
                      ) : logs.length === 0 ? (
                        <tr>
                          <td colSpan={10} className="text-center py-8 text-muted-foreground">
                            暂无数据
                          </td>
                        </tr>
                      ) : (
                        logs.map((row) => (
                          <tr key={row.id} className="border-b hover:bg-accent/30">
                            <td className="px-3 py-2 font-mono text-xs">{row.id}</td>
                            <td className="px-3 py-2 font-mono text-xs whitespace-nowrap">
                              {formatTime(row.ts)}
                            </td>
                            <td className="px-3 py-2 font-mono text-xs">
                              {row.endpoint.replace('/v1/', '')}
                            </td>
                            <td className="px-3 py-2">
                              <Badge variant="secondary" className="font-mono text-[11px]">
                                {row.model || '--'}
                              </Badge>
                              {row.base_model ? (
                                <span className="ml-1 text-xs text-muted-foreground">→ {row.base_model}</span>
                              ) : null}
                            </td>
                            <td className="px-3 py-2 text-center">
                              {row.is_stream ? (
                                <Badge variant="outline" className="text-[10px]">
                                  stream
                                </Badge>
                              ) : (
                                <span className="text-xs text-muted-foreground">--</span>
                              )}
                            </td>
                            <td className="px-3 py-2 text-right font-mono text-xs">
                              {row.prompt_tokens}/{row.completion_tokens}
                              {row.reasoning_tokens > 0 ? ` +${row.reasoning_tokens}` : ''}
                            </td>
                            <td className="px-3 py-2 text-right text-xs whitespace-nowrap">
                              {formatDuration(row.duration_ms)}
                            </td>
                            <td className="px-3 py-2 text-right text-xs text-muted-foreground whitespace-nowrap">
                              {formatBytes(row.request_size)} · {formatBytes(row.response_size)}
                            </td>
                            <td className="px-3 py-2 text-center">
                              {row.has_reasoning ? (
                                <CheckCircle2 className="size-3.5 mx-auto text-emerald-500" />
                              ) : (
                                <span className="text-xs text-muted-foreground">--</span>
                              )}
                            </td>
                            <td className="px-3 py-2 text-center">
                              <Button
                                size="sm"
                                variant="ghost"
                                onClick={() => setDetailId(row.id)}
                                className="h-7 px-2"
                              >
                                <Eye className="size-3.5" />
                                查看
                              </Button>
                            </td>
                          </tr>
                        ))
                      )}
                    </tbody>
                  </table>
                </div>

                {/* 分页 */}
                <div className="flex items-center justify-between mt-4 text-sm">
                  <div className="text-muted-foreground">
                    第 {page + 1} / {Math.max(totalPages, 1)} 页
                  </div>
                  <div className="flex items-center gap-2">
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={page === 0 || logsLoading}
                      onClick={() => setPage((p) => Math.max(0, p - 1))}
                    >
                      <ChevronLeft className="size-3.5" />
                      上一页
                    </Button>
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={page + 1 >= totalPages || logsLoading}
                      onClick={() => setPage((p) => p + 1)}
                    >
                      下一页
                      <ChevronRight className="size-3.5" />
                    </Button>
                  </div>
                </div>
              </CardContent>
            </Card>
          </>
        )}

        <DetailDialog id={detailId} onClose={() => setDetailId(null)} />
      </>
    </StateShell>
  )
}
