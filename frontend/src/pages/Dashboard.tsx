import type { ReactNode } from 'react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import DashboardUsageCharts, { getTimeRangeISO, getBucketConfig } from '../components/DashboardUsageCharts'
import type { TimeRangeKey } from '../components/DashboardUsageCharts'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import StatCard from '../components/StatCard'
import type { StatsResponse, UsageStats, UsageDistribution, ChartAggregation } from '../types'
import { useDataLoader } from '../hooks/useDataLoader'
import { usePolling } from '../hooks/usePolling'
import { Card, CardContent } from '@/components/ui/card'
import { Users, CheckCircle, XCircle, Activity, Zap, Clock, AlertTriangle, BarChart3, Database, Gauge } from 'lucide-react'

const DASHBOARD_REFRESH_INTERVAL_MS = 15_000

export default function Dashboard() {
  const { t } = useTranslation()
  const [timeRange, setTimeRange] = useState<TimeRangeKey>('1h')
  const [chartData, setChartData] = useState<ChartAggregation | null>(null)
  const [chartRefreshedAt, setChartRefreshedAt] = useState<number | null>(null)
  const [chartLoading, setChartLoading] = useState(true)
  const chartAbort = useRef<AbortController | null>(null)

  // 仅加载轻量级统计数据（秒级响应）
  const loadDashboardStats = useCallback(async () => {
    const [stats, usageStats] = await Promise.all([
      api.getStats(),
      api.getUsageStats(),
    ])
    return { stats, usageStats }
  }, [])

  const { data, loading, error, reload, reloadSilently } = useDataLoader<{
    stats: StatsResponse | null
    usageStats: UsageStats | null
  }>({
    initialData: { stats: null, usageStats: null },
    load: loadDashboardStats,
  })

  // 加载服务端聚合的图表数据（12~48 个聚合点，非原始行）
  const loadChartData = useCallback(async () => {
    chartAbort.current?.abort()
    const controller = new AbortController()
    chartAbort.current = controller
    setChartLoading(true)
    try {
      const { start, end } = getTimeRangeISO(timeRange)
      const { bucketMinutes } = getBucketConfig(timeRange)
      const res = await api.getChartData({ start, end, bucketMinutes })
      if (!controller.signal.aborted) {
        setChartData(res)
        setChartRefreshedAt(Date.now())
      }
    } catch {
      // 静默容错
    } finally {
      if (!controller.signal.aborted) {
        setChartLoading(false)
      }
    }
  }, [timeRange])

  // 首次加载 + timeRange 变更时重新拉取图表数据
  useEffect(() => {
    void loadChartData()
  }, [loadChartData])

  // 仅在 1h（实时）模式下启用自动刷新
  usePolling(() => {
    void reloadSilently()
    void loadChartData()
  }, DASHBOARD_REFRESH_INTERVAL_MS, timeRange === '1h')

  const { stats, usageStats } = data
  const total = stats?.total ?? 0
  const available = stats?.available ?? 0
  const errorCount = stats?.error ?? 0
  const todayRequests = stats?.today_requests ?? 0

  const icons: Record<string, ReactNode> = {
    total: <Users className="size-[22px]" />,
    available: <CheckCircle className="size-[22px]" />,
    error: <XCircle className="size-[22px]" />,
    requests: <Activity className="size-[22px]" />,
  }

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => { void reload(); void loadChartData() }}
      loadingTitle={t('dashboard.loadingTitle')}
      loadingDescription={t('dashboard.loadingDesc')}
      errorTitle={t('dashboard.errorTitle')}
    >
      <>
        <PageHeader
          title={t('dashboard.title')}
          description={t('dashboard.description')}
          onRefresh={() => { void reload(); void loadChartData() }}
        />

        {/* Account status */}
        <div className="grid grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-4 mb-6">
          <StatCard icon={icons.total} iconClass="blue" label={t('dashboard.totalAccounts')} value={total} />
          <StatCard
            icon={icons.available}
            iconClass="green"
            label={t('dashboard.available')}
            value={available}
            sub={t('dashboard.availableRate', { rate: total ? Math.round((available / total) * 100) : 0 })}
          />
          <StatCard icon={icons.error} iconClass="red" label={t('dashboard.error')} value={errorCount} />
          <StatCard icon={icons.requests} iconClass="purple" label={t('dashboard.todayRequests')} value={todayRequests} />
        </div>

        {/* Usage distribution */}
        {stats?.usage_distribution && stats.usage_distribution.tracked_count > 0 && (
          <Card className="mb-6">
            <CardContent className="p-6">
              <div className="flex items-center gap-2 mb-4">
                <Gauge className="size-5 text-primary" />
                <h3 className="text-base font-semibold text-foreground">{t('dashboard.usageDistribution')}</h3>
                <span className="text-xs text-muted-foreground ml-auto">
                  {t('dashboard.usageTracked', { count: stats.usage_distribution.tracked_count, total })}
                </span>
              </div>
              <UsageDistBar dist={stats.usage_distribution} />
            </CardContent>
          </Card>
        )}

        {/* Usage stats */}
        {usageStats && (
          <div className="space-y-6">
            <Card>
              <CardContent className="p-6">
                <h3 className="text-base font-semibold text-foreground mb-4">{t('dashboard.usageStats')}</h3>
                <div className="grid grid-cols-[repeat(auto-fit,minmax(200px,1fr))] gap-4">
                  <StatItem icon={<BarChart3 className="size-5" />} iconBg="bg-blue-500/10 text-blue-500" label={t('dashboard.totalRequests')} value={usageStats.total_requests.toLocaleString()} />
                  <StatItem icon={<Zap className="size-5" />} iconBg="bg-purple-500/10 text-purple-500" label={t('dashboard.totalTokens')} value={usageStats.total_tokens.toLocaleString()} />
                  <StatItem icon={<Zap className="size-5" />} iconBg="bg-emerald-500/10 text-emerald-500" label={t('dashboard.todayTokens')} value={usageStats.today_tokens.toLocaleString()} />
                  <StatItem icon={<Database className="size-5" />} iconBg="bg-indigo-500/10 text-indigo-500" label={t('dashboard.cachedTokens')} value={usageStats.total_cached_tokens.toLocaleString()} />
                  <StatItem icon={<Activity className="size-5" />} iconBg="bg-amber-500/10 text-amber-500" label={t('dashboard.rpmTpm')} value={`${usageStats.rpm} / ${usageStats.tpm.toLocaleString()}`} />
                  <StatItem
                    icon={<Clock className="size-5" />}
                    iconBg="bg-cyan-500/10 text-cyan-500"
                    label={t('dashboard.avgLatency')}
                    value={usageStats.avg_duration_ms > 1000 ? `${(usageStats.avg_duration_ms / 1000).toFixed(1)}s` : `${Math.round(usageStats.avg_duration_ms)}ms`}
                  />
                  <StatItem icon={<AlertTriangle className="size-5" />} iconBg="bg-red-500/10 text-red-500" label={t('dashboard.todayErrorRate')} value={`${usageStats.error_rate.toFixed(1)}%`} />
                </div>
              </CardContent>
            </Card>
            <DashboardUsageCharts
              chartData={chartData}
              refreshedAt={chartRefreshedAt}
              refreshIntervalMs={DASHBOARD_REFRESH_INTERVAL_MS}
              timeRange={timeRange}
              onTimeRangeChange={setTimeRange}
              loading={chartLoading}
            />
          </div>
        )}
      </>
    </StateShell>
  )
}

function StatItem({ icon, iconBg, label, value }: { icon: ReactNode; iconBg: string; label: string; value: string }) {
  return (
    <div className="flex items-center gap-3 p-4 rounded-xl bg-muted/50">
      <div className={`flex items-center justify-center size-10 rounded-lg ${iconBg}`}>
        {icon}
      </div>
      <div>
        <div className="text-xs text-muted-foreground">{label}</div>
        <div className="text-lg font-bold">{value}</div>
      </div>
    </div>
  )
}

const USAGE_TIERS = [
  { key: 'low', color: 'bg-emerald-500', label: '0-25%' },
  { key: 'medium', color: 'bg-blue-500', label: '25-50%' },
  { key: 'high', color: 'bg-amber-500', label: '50-75%' },
  { key: 'critical', color: 'bg-orange-500', label: '75-100%' },
  { key: 'exhausted', color: 'bg-red-500', label: '≥100%' },
] as const

function UsageDistBar({ dist }: { dist: UsageDistribution }) {
  const { t } = useTranslation()
  const total = dist.tracked_count
  const avgColor = dist.avg_percent >= 100 ? 'text-red-500' : dist.avg_percent >= 75 ? 'text-orange-500' : dist.avg_percent >= 50 ? 'text-amber-500' : 'text-emerald-500'

  return (
    <div className="space-y-4">
      {/* Average usage */}
      <div className="flex items-center gap-4">
        <div className="flex-1">
          <div className="flex items-baseline gap-2 mb-1">
            <span className={`text-2xl font-bold ${avgColor}`}>{dist.avg_percent.toFixed(1)}%</span>
            <span className="text-xs text-muted-foreground">{t('dashboard.usageAvg7d')}</span>
          </div>
          <div className="w-full h-2 rounded-full bg-muted overflow-hidden">
            <div
              className={`h-full rounded-full transition-all ${dist.avg_percent >= 100 ? 'bg-red-500' : dist.avg_percent >= 75 ? 'bg-orange-500' : dist.avg_percent >= 50 ? 'bg-amber-500' : 'bg-emerald-500'}`}
              style={{ width: `${Math.min(dist.avg_percent, 100)}%` }}
            />
          </div>
        </div>
      </div>

      {/* Segmented bar */}
      {total > 0 && (
        <div>
          <div className="flex w-full h-3 rounded-full overflow-hidden bg-muted">
            {USAGE_TIERS.map(({ key, color }) => {
              const count = dist[key]
              if (count === 0) return null
              return (
                <div
                  key={key}
                  className={`${color} transition-all`}
                  style={{ width: `${(count / total) * 100}%` }}
                  title={`${key}: ${count}`}
                />
              )
            })}
          </div>
          <div className="flex flex-wrap gap-x-4 gap-y-1 mt-2">
            {USAGE_TIERS.map(({ key, color, label }) => {
              const count = dist[key]
              return (
                <div key={key} className="flex items-center gap-1.5 text-xs">
                  <div className={`size-2.5 rounded-full ${color}`} />
                  <span className="text-muted-foreground">{label}</span>
                  <span className="font-semibold">{count}</span>
                </div>
              )
            })}
          </div>
        </div>
      )}
    </div>
  )
}
