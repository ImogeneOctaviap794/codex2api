import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  Legend,
  Line,
  LineChart,
  Pie,
  PieChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { api } from '../api'
import { getTimeRangeISO, getBucketConfig } from '../components/DashboardUsageCharts'
import type { TimeRangeKey } from '../components/DashboardUsageCharts'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import { usePolling } from '../hooks/usePolling'
import { Card, CardContent } from '@/components/ui/card'
import {
  BarChart3,
  Zap,
  Activity,
  Clock,
  AlertTriangle,
  TrendingUp,
  Database,
  Cpu,
  Layers,
  PieChart as PieChartIcon,
  ArrowUpRight,
  ArrowDownRight,
} from 'lucide-react'
import type { StatsResponse, UsageStats, ChartAggregation, OpsOverviewResponse } from '../types'

const AUTO_REFRESH_MS = 30_000
const TIME_RANGE_OPTIONS: TimeRangeKey[] = ['1h', '6h', '24h', '7d', '30d']

const DONUT_COLORS = [
  'hsl(258, 60%, 63%)',
  'hsl(210, 80%, 60%)',
  'hsl(152, 55%, 46%)',
  'hsl(36, 80%, 50%)',
  'hsl(0, 65%, 54%)',
  'hsl(280, 60%, 60%)',
  'hsl(190, 70%, 50%)',
  'hsl(330, 60%, 55%)',
]

const TOKEN_COLORS = {
  input: 'hsl(220, 55%, 60%)',
  output: 'hsl(152, 55%, 46%)',
  reasoning: 'hsl(36, 80%, 50%)',
  cached: 'hsl(258, 60%, 63%)',
}

const chartMargin = { top: 8, right: 16, left: -8, bottom: 0 }
const gridColor = 'var(--color-border)'
const axisColor = 'var(--color-muted-foreground)'
const tooltipStyle = {
  backgroundColor: 'var(--color-card)',
  border: '1px solid var(--color-border)',
  borderRadius: '16px',
  boxShadow: '0 18px 40px rgba(0, 0, 0, 0.12)',
}
const tooltipLabelStyle = { color: 'var(--color-foreground)', fontWeight: 600 }
const tooltipItemStyle = { color: 'var(--color-foreground)' }
const compactFmt = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 })

export default function Analytics() {
  const { t } = useTranslation()
  const [timeRange, setTimeRange] = useState<TimeRangeKey>('24h')
  const [chartData, setChartData] = useState<ChartAggregation | null>(null)
  const [chartLoading, setChartLoading] = useState(true)
  const chartAbort = useRef<AbortController | null>(null)

  const loadStats = useCallback(async () => {
    const [stats, usageStats, ops] = await Promise.all([
      api.getStats(),
      api.getUsageStats(),
      api.getOpsOverview().catch(() => null),
    ])
    return { stats, usageStats, ops }
  }, [])

  const { data, loading, error, reload, reloadSilently } = useDataLoader<{
    stats: StatsResponse | null
    usageStats: UsageStats | null
    ops: OpsOverviewResponse | null
  }>({
    initialData: { stats: null, usageStats: null, ops: null },
    load: loadStats,
  })

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
      }
    } catch { /* silent */ } finally {
      if (!controller.signal.aborted) setChartLoading(false)
    }
  }, [timeRange])

  useEffect(() => { void loadChartData() }, [loadChartData])

  usePolling(() => {
    void reloadSilently()
    void loadChartData()
  }, AUTO_REFRESH_MS)

  const { stats, usageStats, ops } = data

  // Derived data
  const totalRequests = usageStats?.total_requests ?? 0
  const totalTokens = usageStats?.total_tokens ?? 0
  const todayRequests = usageStats?.today_requests ?? 0
  const todayTokens = usageStats?.today_tokens ?? 0
  const errorRate = usageStats?.error_rate ?? 0
  const avgLatency = usageStats?.avg_duration_ms ?? 0
  const rpm = usageStats?.rpm ?? 0
  const tpm = usageStats?.tpm ?? 0
  const totalAccounts = stats?.total ?? 0
  const availableAccounts = stats?.available ?? 0
  const errorAccounts = stats?.error ?? 0

  // Token breakdown for donut
  const tokenBreakdown = usageStats ? [
    { name: t('analytics.inputTokens'), value: usageStats.total_prompt_tokens, color: TOKEN_COLORS.input },
    { name: t('analytics.outputTokens'), value: usageStats.total_completion_tokens, color: TOKEN_COLORS.output },
    { name: t('analytics.cachedTokens'), value: usageStats.total_cached_tokens, color: TOKEN_COLORS.cached },
  ].filter(d => d.value > 0) : []

  // Model distribution for donut
  const modelData = chartData?.models
    ?.slice()
    .sort((a, b) => b.requests - a.requests)
    .slice(0, 8) ?? []

  // Timeline
  const timelineData = chartData?.timeline?.map(p => {
    const d = new Date(p.bucket)
    return {
      label: formatTimeLabel(d, timeRange),
      fullLabel: formatFullLabel(d),
      requests: p.requests,
      avgLatency: p.avg_latency > 0 ? Math.round(p.avg_latency) : null,
      inputTokens: p.input_tokens,
      outputTokens: p.output_tokens,
      reasoningTokens: p.reasoning_tokens,
      cachedTokens: p.cached_tokens,
      errors401: p.errors_401,
      totalTokens: p.input_tokens + p.output_tokens + p.reasoning_tokens + p.cached_tokens,
    }
  }) ?? []

  const totalTimelineRequests = timelineData.reduce((s, p) => s + p.requests, 0)

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => { void reload(); void loadChartData() }}
      loadingTitle={t('analytics.loadingTitle')}
      loadingDescription={t('analytics.loadingDesc')}
      errorTitle={t('analytics.errorTitle')}
    >
      <>
        <PageHeader
          title={t('analytics.title')}
          description={t('analytics.description')}
          onRefresh={() => { void reload(); void loadChartData() }}
        />

        {/* ─── Hero Stats ─── */}
        <div className="grid grid-cols-2 lg:grid-cols-4 gap-4 mb-8">
          <HeroCard
            icon={<BarChart3 className="size-5" />}
            gradient="from-blue-500/20 to-indigo-500/10"
            iconBg="bg-blue-500/14 text-blue-500"
            label={t('analytics.totalRequests')}
            value={formatBigNumber(totalRequests)}
            sub={`${t('analytics.today')}: ${formatBigNumber(todayRequests)}`}
            trend={todayRequests > 0 ? 'up' : undefined}
          />
          <HeroCard
            icon={<Zap className="size-5" />}
            gradient="from-purple-500/20 to-pink-500/10"
            iconBg="bg-purple-500/14 text-purple-500"
            label={t('analytics.totalTokens')}
            value={formatBigNumber(totalTokens)}
            sub={`${t('analytics.today')}: ${formatBigNumber(todayTokens)}`}
            trend={todayTokens > 0 ? 'up' : undefined}
          />
          <HeroCard
            icon={<Activity className="size-5" />}
            gradient="from-emerald-500/20 to-teal-500/10"
            iconBg="bg-emerald-500/14 text-emerald-500"
            label={t('analytics.rpmTpm')}
            value={`${rpm}`}
            sub={`TPM: ${formatBigNumber(tpm)}`}
          />
          <HeroCard
            icon={<AlertTriangle className="size-5" />}
            gradient="from-amber-500/20 to-orange-500/10"
            iconBg="bg-amber-500/14 text-amber-500"
            label={t('analytics.errorRate')}
            value={`${errorRate.toFixed(1)}%`}
            sub={avgLatency > 1000
              ? `${t('analytics.avgLatency')}: ${(avgLatency / 1000).toFixed(1)}s`
              : `${t('analytics.avgLatency')}: ${Math.round(avgLatency)}ms`}
            trend={errorRate > 5 ? 'down' : undefined}
          />
        </div>

        {/* ─── Account Health Bar ─── */}
        {stats && (
          <Card className="mb-8 py-0 overflow-hidden">
            <CardContent className="p-6">
              <div className="flex items-center justify-between mb-4">
                <div className="flex items-center gap-2">
                  <Cpu className="size-4 text-muted-foreground" />
                  <h3 className="text-sm font-semibold text-foreground">{t('analytics.accountHealth')}</h3>
                </div>
                <span className="text-xs text-muted-foreground">
                  {availableAccounts} / {totalAccounts} {t('analytics.accountsAvailable')}
                </span>
              </div>
              <div className="flex gap-1 h-4 rounded-full overflow-hidden bg-muted/50">
                {totalAccounts > 0 && (
                  <>
                    <div
                      className="bg-emerald-500 rounded-l-full transition-all duration-700"
                      style={{ width: `${(availableAccounts / totalAccounts) * 100}%` }}
                      title={`${t('analytics.available')}: ${availableAccounts}`}
                    />
                    {errorAccounts > 0 && (
                      <div
                        className="bg-red-500 transition-all duration-700"
                        style={{ width: `${(errorAccounts / totalAccounts) * 100}%` }}
                        title={`${t('analytics.errorAccounts')}: ${errorAccounts}`}
                      />
                    )}
                    {(totalAccounts - availableAccounts - errorAccounts) > 0 && (
                      <div
                        className="bg-amber-500 transition-all duration-700"
                        style={{ width: `${((totalAccounts - availableAccounts - errorAccounts) / totalAccounts) * 100}%` }}
                        title={`${t('analytics.other')}: ${totalAccounts - availableAccounts - errorAccounts}`}
                      />
                    )}
                  </>
                )}
              </div>
              <div className="flex gap-6 mt-3 text-xs text-muted-foreground">
                <span className="flex items-center gap-1.5"><span className="size-2.5 rounded-full bg-emerald-500" /> {t('analytics.available')} ({availableAccounts})</span>
                <span className="flex items-center gap-1.5"><span className="size-2.5 rounded-full bg-red-500" /> {t('analytics.errorAccounts')} ({errorAccounts})</span>
                <span className="flex items-center gap-1.5"><span className="size-2.5 rounded-full bg-amber-500" /> {t('analytics.other')} ({Math.max(0, totalAccounts - availableAccounts - errorAccounts)})</span>
              </div>
            </CardContent>
          </Card>
        )}

        {/* ─── Time Range Selector ─── */}
        <div className="flex items-center justify-between mb-6 flex-wrap gap-4">
          <div>
            <h3 className="text-base font-semibold text-foreground">{t('analytics.trendCharts')}</h3>
            <p className="text-sm text-muted-foreground mt-1">
              {t('analytics.trendChartsDesc', { count: totalTimelineRequests.toLocaleString() })}
            </p>
          </div>
          <div className="inline-flex rounded-lg border border-border bg-muted/50 p-0.5">
            {TIME_RANGE_OPTIONS.map(key => (
              <button
                key={key}
                onClick={() => setTimeRange(key)}
                className={`px-3 py-1.5 text-xs font-medium rounded-md transition-all duration-200 ${
                  timeRange === key
                    ? 'bg-background text-foreground shadow-sm border border-border'
                    : 'text-muted-foreground hover:text-foreground'
                }`}
              >
                {t(`dashboard.timeRange${key.toUpperCase()}`)}
              </button>
            ))}
          </div>
        </div>

        {/* ─── Charts Grid ─── */}
        {chartLoading ? (
          <div className="grid grid-cols-1 xl:grid-cols-2 gap-4 mb-8">
            {[0, 1, 2, 3].map(i => (
              <Card key={i} className="py-0">
                <CardContent className="p-6">
                  <div className="mb-5 space-y-2">
                    <div className="h-4 w-32 rounded-md bg-muted animate-pulse" />
                    <div className="h-3 w-48 rounded-md bg-muted/60 animate-pulse" />
                  </div>
                  <div className="h-[280px] flex items-end gap-2 px-4 pb-4">
                    {[40, 65, 30, 80, 55, 70, 45, 60, 35, 75, 50, 68].map((h, j) => (
                      <div key={j} className="flex-1 rounded-t-md bg-muted/50 animate-pulse" style={{ height: `${h}%`, animationDelay: `${j * 80}ms` }} />
                    ))}
                  </div>
                </CardContent>
              </Card>
            ))}
          </div>
        ) : (
          <div className="grid grid-cols-1 xl:grid-cols-2 gap-4 mb-8">
            {/* Request Trend */}
            <ChartCard icon={<TrendingUp className="size-4" />} title={t('analytics.requestTrend')} description={t('analytics.requestTrendDesc')}>
              <ResponsiveContainer width="100%" height="100%">
                <AreaChart data={timelineData} margin={chartMargin}>
                  <defs>
                    <linearGradient id="analytics-req-grad" x1="0" y1="0" x2="0" y2="1">
                      <stop offset="5%" stopColor="var(--color-primary)" stopOpacity={0.3} />
                      <stop offset="95%" stopColor="var(--color-primary)" stopOpacity={0} />
                    </linearGradient>
                  </defs>
                  <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="4 4" />
                  <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 11 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} minTickGap={20} tickMargin={8} />
                  <YAxis tickFormatter={fmtCompact} tick={{ fill: axisColor, fontSize: 11 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} allowDecimals={false} />
                  <Tooltip
                    formatter={fmtTooltipVal}
                    labelFormatter={(_, p) => tooltipLabel(p, 'fullLabel')}
                    contentStyle={tooltipStyle}
                    labelStyle={tooltipLabelStyle}
                    itemStyle={tooltipItemStyle}
                  />
                  <Area type="monotone" dataKey="requests" name={t('analytics.requests')} stroke="var(--color-primary)" fill="url(#analytics-req-grad)" strokeWidth={2.5} />
                </AreaChart>
              </ResponsiveContainer>
            </ChartCard>

            {/* Token Volume Trend */}
            <ChartCard icon={<Layers className="size-4" />} title={t('analytics.tokenTrend')} description={t('analytics.tokenTrendDesc')}>
              <ResponsiveContainer width="100%" height="100%">
                <BarChart data={timelineData} margin={chartMargin}>
                  <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="4 4" />
                  <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 11 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} minTickGap={20} tickMargin={8} />
                  <YAxis tickFormatter={fmtCompact} tick={{ fill: axisColor, fontSize: 11 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} />
                  <Tooltip
                    formatter={fmtTooltipVal}
                    labelFormatter={(_, p) => tooltipLabel(p, 'fullLabel')}
                    contentStyle={tooltipStyle}
                    labelStyle={tooltipLabelStyle}
                    itemStyle={tooltipItemStyle}
                  />
                  <Legend wrapperStyle={{ paddingTop: 8, fontSize: 11 }} />
                  <Bar dataKey="inputTokens" stackId="t" name={t('analytics.inputTokens')} fill={TOKEN_COLORS.input} radius={[0, 0, 4, 4]} />
                  <Bar dataKey="outputTokens" stackId="t" name={t('analytics.outputTokens')} fill={TOKEN_COLORS.output} />
                  <Bar dataKey="reasoningTokens" stackId="t" name={t('analytics.reasoningTokens')} fill={TOKEN_COLORS.reasoning} />
                  <Bar dataKey="cachedTokens" stackId="t" name={t('analytics.cachedTokens')} fill={TOKEN_COLORS.cached} radius={[4, 4, 0, 0]} />
                </BarChart>
              </ResponsiveContainer>
            </ChartCard>

            {/* Latency Trend */}
            <ChartCard icon={<Clock className="size-4" />} title={t('analytics.latencyTrend')} description={t('analytics.latencyTrendDesc')}>
              <ResponsiveContainer width="100%" height="100%">
                <LineChart data={timelineData} margin={chartMargin}>
                  <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="4 4" />
                  <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 11 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} minTickGap={20} tickMargin={8} />
                  <YAxis tickFormatter={fmtDuration} tick={{ fill: axisColor, fontSize: 11 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} width={54} />
                  <Tooltip
                    formatter={(v: unknown) => fmtDurationFull(v)}
                    labelFormatter={(_, p) => tooltipLabel(p, 'fullLabel')}
                    contentStyle={tooltipStyle}
                    labelStyle={tooltipLabelStyle}
                    itemStyle={tooltipItemStyle}
                  />
                  <Line
                    type="monotone"
                    dataKey="avgLatency"
                    name={t('analytics.avgLatencyLine')}
                    stroke="hsl(var(--info))"
                    strokeWidth={2.5}
                    dot={false}
                    connectNulls
                    activeDot={{ r: 4 }}
                  />
                </LineChart>
              </ResponsiveContainer>
            </ChartCard>

            {/* Error Trend */}
            <ChartCard icon={<AlertTriangle className="size-4" />} title={t('analytics.errorTrend')} description={t('analytics.errorTrendDesc')}>
              <ResponsiveContainer width="100%" height="100%">
                <AreaChart data={timelineData} margin={chartMargin}>
                  <defs>
                    <linearGradient id="analytics-err-grad" x1="0" y1="0" x2="0" y2="1">
                      <stop offset="5%" stopColor="var(--color-destructive)" stopOpacity={0.3} />
                      <stop offset="95%" stopColor="var(--color-destructive)" stopOpacity={0} />
                    </linearGradient>
                  </defs>
                  <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="4 4" />
                  <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 11 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} minTickGap={20} tickMargin={8} />
                  <YAxis tickFormatter={fmtCompact} tick={{ fill: axisColor, fontSize: 11 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} allowDecimals={false} />
                  <Tooltip
                    formatter={fmtTooltipVal}
                    labelFormatter={(_, p) => tooltipLabel(p, 'fullLabel')}
                    contentStyle={tooltipStyle}
                    labelStyle={tooltipLabelStyle}
                    itemStyle={tooltipItemStyle}
                  />
                  <Area type="monotone" dataKey="errors401" name={t('analytics.errors401')} stroke="var(--color-destructive)" fill="url(#analytics-err-grad)" strokeWidth={2} />
                </AreaChart>
              </ResponsiveContainer>
            </ChartCard>
          </div>
        )}

        {/* ─── Donut Charts Row ─── */}
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-4 mb-8">
          {/* Token Type Distribution */}
          {tokenBreakdown.length > 0 && (
            <Card className="py-0">
              <CardContent className="p-6">
                <div className="flex items-center gap-2 mb-4">
                  <Database className="size-4 text-muted-foreground" />
                  <h4 className="text-sm font-semibold text-foreground">{t('analytics.tokenDistribution')}</h4>
                </div>
                <div className="flex items-center gap-6">
                  <div className="w-[200px] h-[200px] shrink-0">
                    <ResponsiveContainer width="100%" height="100%">
                      <PieChart>
                        <Pie
                          data={tokenBreakdown}
                          cx="50%"
                          cy="50%"
                          innerRadius={55}
                          outerRadius={85}
                          paddingAngle={3}
                          dataKey="value"
                          strokeWidth={0}
                        >
                          {tokenBreakdown.map((entry, i) => (
                            <Cell key={i} fill={entry.color} />
                          ))}
                        </Pie>
                        <Tooltip
                          formatter={(value: unknown) => (typeof value === 'number' ? value : Number(value)).toLocaleString()}
                          contentStyle={tooltipStyle}
                          itemStyle={tooltipItemStyle}
                        />
                      </PieChart>
                    </ResponsiveContainer>
                  </div>
                  <div className="flex flex-col gap-3 flex-1 min-w-0">
                    {tokenBreakdown.map((item, i) => (
                      <div key={i} className="flex items-center justify-between gap-2">
                        <div className="flex items-center gap-2 min-w-0">
                          <span className="size-3 rounded-full shrink-0" style={{ backgroundColor: item.color }} />
                          <span className="text-sm text-muted-foreground truncate">{item.name}</span>
                        </div>
                        <div className="text-sm font-semibold text-foreground whitespace-nowrap">
                          {formatBigNumber(item.value)}
                        </div>
                      </div>
                    ))}
                    <div className="pt-2 mt-1 border-t border-border flex items-center justify-between">
                      <span className="text-xs font-medium text-muted-foreground">{t('analytics.totalTokens')}</span>
                      <span className="text-sm font-bold text-foreground">{formatBigNumber(totalTokens)}</span>
                    </div>
                  </div>
                </div>
              </CardContent>
            </Card>
          )}

          {/* Model Distribution */}
          {modelData.length > 0 && (
            <Card className="py-0">
              <CardContent className="p-6">
                <div className="flex items-center gap-2 mb-4">
                  <PieChartIcon className="size-4 text-muted-foreground" />
                  <h4 className="text-sm font-semibold text-foreground">{t('analytics.modelDistribution')}</h4>
                </div>
                <div className="flex items-center gap-6">
                  <div className="w-[200px] h-[200px] shrink-0">
                    <ResponsiveContainer width="100%" height="100%">
                      <PieChart>
                        <Pie
                          data={modelData}
                          cx="50%"
                          cy="50%"
                          innerRadius={55}
                          outerRadius={85}
                          paddingAngle={3}
                          dataKey="requests"
                          strokeWidth={0}
                        >
                          {modelData.map((_, i) => (
                            <Cell key={i} fill={DONUT_COLORS[i % DONUT_COLORS.length]} />
                          ))}
                        </Pie>
                        <Tooltip
                          formatter={(value: unknown) => (typeof value === 'number' ? value : Number(value)).toLocaleString()}
                          contentStyle={tooltipStyle}
                          itemStyle={tooltipItemStyle}
                        />
                      </PieChart>
                    </ResponsiveContainer>
                  </div>
                  <div className="flex flex-col gap-2 flex-1 min-w-0">
                    {modelData.map((item, i) => {
                      const totalModelReqs = modelData.reduce((s, m) => s + m.requests, 0)
                      const pct = totalModelReqs > 0 ? ((item.requests / totalModelReqs) * 100).toFixed(1) : '0'
                      return (
                        <div key={i} className="flex items-center gap-2">
                          <span className="size-2.5 rounded-full shrink-0" style={{ backgroundColor: DONUT_COLORS[i % DONUT_COLORS.length] }} />
                          <span className="text-xs text-muted-foreground truncate flex-1 min-w-0" title={item.model}>
                            {truncate(item.model, 24)}
                          </span>
                          <span className="text-xs font-semibold text-foreground whitespace-nowrap">{pct}%</span>
                        </div>
                      )
                    })}
                  </div>
                </div>
              </CardContent>
            </Card>
          )}
        </div>

        {/* ─── Real-time Gauges ─── */}
        {ops && (
          <Card className="py-0 mb-8">
            <CardContent className="p-6">
              <h3 className="text-sm font-semibold text-foreground mb-5 flex items-center gap-2">
                <Activity className="size-4 text-muted-foreground" />
                {t('analytics.realTimeMetrics')}
              </h3>
              <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-6 gap-4">
                <GaugeCard label="QPS" value={ops.traffic.qps.toFixed(1)} sub={`${t('analytics.peak')}: ${ops.traffic.qps_peak.toFixed(1)}`} percent={ops.traffic.qps_peak > 0 ? (ops.traffic.qps / ops.traffic.qps_peak) * 100 : 0} color="bg-blue-500" />
                <GaugeCard label="TPS" value={fmtCompact(ops.traffic.tps)} sub={`${t('analytics.peak')}: ${fmtCompact(ops.traffic.tps_peak)}`} percent={ops.traffic.tps_peak > 0 ? (ops.traffic.tps / ops.traffic.tps_peak) * 100 : 0} color="bg-purple-500" />
                <GaugeCard label="RPM" value={String(ops.traffic.rpm)} sub={ops.traffic.rpm_limit > 0 ? `${t('analytics.limit')}: ${ops.traffic.rpm_limit}` : t('analytics.noLimit')} percent={ops.traffic.rpm_limit > 0 ? (ops.traffic.rpm / ops.traffic.rpm_limit) * 100 : 0} color="bg-emerald-500" />
                <GaugeCard label="TPM" value={fmtCompact(ops.traffic.tpm)} sub={`${t('analytics.today')}: ${formatBigNumber(ops.traffic.today_tokens)}`} percent={0} color="bg-amber-500" />
                <GaugeCard label="CPU" value={`${ops.cpu.percent.toFixed(0)}%`} sub={`${ops.cpu.cores} ${t('analytics.cores')}`} percent={ops.cpu.percent} color={ops.cpu.percent > 80 ? 'bg-red-500' : 'bg-blue-500'} />
                <GaugeCard label={t('analytics.memory')} value={`${ops.memory.percent.toFixed(0)}%`} sub={formatBytes(ops.memory.used_bytes)} percent={ops.memory.percent} color={ops.memory.percent > 80 ? 'bg-red-500' : 'bg-emerald-500'} />
              </div>
            </CardContent>
          </Card>
        )}
      </>
    </StateShell>
  )
}

// ─── Sub-components ───

function HeroCard({ icon, gradient, iconBg, label, value, sub, trend }: {
  icon: React.ReactNode
  gradient: string
  iconBg: string
  label: string
  value: string
  sub: string
  trend?: 'up' | 'down'
}) {
  return (
    <Card className={`relative overflow-hidden py-0 transition-all duration-200 hover:-translate-y-0.5 hover:shadow-lg`}>
      <div className={`absolute inset-0 bg-gradient-to-br ${gradient} pointer-events-none`} />
      <CardContent className="relative p-5">
        <div className="flex items-center justify-between mb-3">
          <div className={`size-9 flex items-center justify-center rounded-xl ${iconBg}`}>
            {icon}
          </div>
          {trend && (
            <span className={`flex items-center gap-0.5 text-xs font-medium ${trend === 'up' ? 'text-emerald-500' : 'text-red-500'}`}>
              {trend === 'up' ? <ArrowUpRight className="size-3.5" /> : <ArrowDownRight className="size-3.5" />}
            </span>
          )}
        </div>
        <div className="text-[11px] font-bold tracking-[0.12em] uppercase text-muted-foreground mb-1">{label}</div>
        <div className="text-[28px] font-bold leading-none tracking-tighter text-foreground">{value}</div>
        <div className="mt-2 text-xs text-muted-foreground">{sub}</div>
      </CardContent>
    </Card>
  )
}

function ChartCard({ icon, title, description, children }: {
  icon: React.ReactNode
  title: string
  description: string
  children: React.ReactNode
}) {
  return (
    <Card className="py-0">
      <CardContent className="p-6">
        <div className="mb-5">
          <h4 className="text-sm font-semibold text-foreground flex items-center gap-2">{icon} {title}</h4>
          <p className="mt-1 text-xs text-muted-foreground">{description}</p>
        </div>
        <div className="h-[280px]">{children}</div>
      </CardContent>
    </Card>
  )
}

function GaugeCard({ label, value, sub, percent, color }: {
  label: string
  value: string
  sub: string
  percent: number
  color: string
}) {
  const clampedPercent = Math.min(100, Math.max(0, percent))
  return (
    <div className="flex flex-col gap-2 p-4 rounded-xl bg-muted/40">
      <span className="text-[10px] font-bold tracking-wider uppercase text-muted-foreground">{label}</span>
      <span className="text-xl font-bold text-foreground">{value}</span>
      {clampedPercent > 0 && (
        <div className="h-1.5 rounded-full bg-muted/80 overflow-hidden">
          <div className={`h-full rounded-full ${color} transition-all duration-700`} style={{ width: `${clampedPercent}%` }} />
        </div>
      )}
      <span className="text-[10px] text-muted-foreground">{sub}</span>
    </div>
  )
}

// ─── Helpers ───

function formatBigNumber(value: number): string {
  if (value >= 1_000_000_000) return `${(value / 1_000_000_000).toFixed(1)}B`
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`
  if (value >= 10_000) return `${(value / 1_000).toFixed(1)}K`
  return value.toLocaleString()
}

function formatBytes(bytes: number): string {
  if (bytes >= 1073741824) return `${(bytes / 1073741824).toFixed(1)} GB`
  if (bytes >= 1048576) return `${(bytes / 1048576).toFixed(0)} MB`
  return `${(bytes / 1024).toFixed(0)} KB`
}

function formatTimeLabel(date: Date, range: TimeRangeKey): string {
  const h = String(date.getHours()).padStart(2, '0')
  const m = String(date.getMinutes()).padStart(2, '0')
  if (range === '7d' || range === '30d') {
    const mon = String(date.getMonth() + 1).padStart(2, '0')
    const day = String(date.getDate()).padStart(2, '0')
    return range === '30d' ? `${mon}-${day}` : `${mon}-${day} ${h}:00`
  }
  return `${h}:${m}`
}

function formatFullLabel(date: Date): string {
  const mon = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  const h = String(date.getHours()).padStart(2, '0')
  const m = String(date.getMinutes()).padStart(2, '0')
  return `${mon}-${day} ${h}:${m}`
}

function truncate(s: string, max: number): string {
  return s.length <= max ? s : s.slice(0, max - 1) + '…'
}

function fmtCompact(value: number | string): string {
  const n = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(n)) return '0'
  return compactFmt.format(n)
}

function fmtTooltipVal(value: unknown): string {
  const n = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(n)) return '0'
  return n.toLocaleString()
}

function fmtDuration(value: number | string): string {
  const n = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(n)) return '0ms'
  if (n >= 1000) return `${(n / 1000).toFixed(1)}s`
  return `${Math.round(n)}ms`
}

function fmtDurationFull(value: unknown): string {
  const n = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(n) || n <= 0) return '-'
  if (n >= 1000) return `${(n / 1000).toFixed(1)}s`
  return `${Math.round(n)}ms`
}

function tooltipLabel(payload: readonly { payload?: Record<string, unknown> }[] | undefined, key: string): string {
  const val = payload?.[0]?.payload?.[key]
  return typeof val === 'string' ? val : ''
}
