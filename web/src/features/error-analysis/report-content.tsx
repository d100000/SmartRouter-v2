/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import {
  BarChart3,
  FileText,
  Paperclip,
  Pin,
  Search,
  TrendingUp,
  Wrench,
} from 'lucide-react'
import { lazy, Suspense, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table'
import { LoadingState } from '@/components/loading-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber, formatTimestamp } from '@/lib/format'

import { useAnalysisLabels } from './labels'
import { RequestEvidenceDialog } from './request-evidence-dialog'
import type {
  AnalysisSeverity,
  AnalysisTask,
  FailureCounts,
  FailureLog,
} from './types'

const severityClass: Record<AnalysisSeverity, string> = {
  critical: 'bg-destructive/15 text-destructive',
  high: 'bg-orange-500/15 text-orange-700 dark:text-orange-400',
  medium: 'bg-amber-500/15 text-amber-700 dark:text-amber-400',
  low: 'bg-muted text-muted-foreground',
}

const severityOrder: AnalysisSeverity[] = ['critical', 'high', 'medium', 'low']

function useSeverityLabels(): Record<AnalysisSeverity, string> {
  const { t } = useTranslation()
  return {
    critical: t('Critical'),
    high: t('High'),
    medium: t('Medium'),
    low: t('Low'),
  }
}

function SeverityBadge(props: { severity?: string }) {
  const labels = useSeverityLabels()
  const severity = props.severity as AnalysisSeverity | undefined
  if (!severity || !(severity in severityClass)) return null
  return <Badge className={severityClass[severity]}>{labels[severity]}</Badge>
}

const Markdown = lazy(() =>
  import('@/components/ui/markdown').then((module) => ({
    default: module.Markdown,
  }))
)
const TrendChart = lazy(() => import('./trend-chart'))
const sectionIcons = {
  priority: Search,
  distribution: BarChart3,
  trends: TrendingUp,
  actions: Wrench,
  evidence: Paperclip,
  limits: Paperclip,
}

export function ReportContent(props: { task: AnalysisTask }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const labels = useAnalysisLabels()
  const severityLabels = useSeverityLabels()
  const [detail, setDetail] = useState<{
    row?: FailureLog
    requestID?: string
  } | null>(null)
  const [filter, setFilter] = useState({ severity: '', channel: '', model: '' })
  const result = props.task.result
  // Issues can be narrowed by severity, channel and model. A filter value the
  // shown report does not offer is ignored, so switching reports resets it.
  const sections = result?.sections ?? []
  const issues = sections.filter((section) => section.id.startsWith('issue-'))
  const severities = severityOrder.filter((value) =>
    issues.some((issue) => issue.severity === value)
  )
  const channels = [...new Set(issues.flatMap((issue) => issue.channels ?? []))]
  const models = [...new Set(issues.flatMap((issue) => issue.models ?? []))]
  const severity = severities.find((value) => value === filter.severity) ?? ''
  const channel = channels.includes(filter.channel) ? filter.channel : ''
  const model = models.includes(filter.model) ? filter.model : ''
  const visibleSections = sections.filter(
    (section) =>
      !section.id.startsWith('issue-') ||
      ((!severity || section.severity === severity) &&
        (!channel || (section.channels ?? []).includes(channel)) &&
        (!model || (section.models ?? []).includes(model)))
  )
  const visibleIssues = visibleSections.filter((section) =>
    section.id.startsWith('issue-')
  ).length
  const current = result?.snapshot
  const previous = result?.comparison
  const versioned = result?.version === 2
  const compared = result?.comparison_available === true && current && previous
  const date = new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeStyle: 'short',
    timeZone: result?.timezone,
  })
  const categories: { key: keyof FailureCounts; label: string }[] = [
    { key: 'affected', label: t('Affected requests') },
    { key: 'zero_output', label: t('Zero output') },
    { key: 'interrupted', label: t('Interrupted streams') },
    { key: 'errors', label: t('Request errors') },
    { key: 'client_cancelled', label: t('Client cancelled') },
    { key: 'recovered', label: t('Recovered after retry') },
  ]

  return (
    <div className='min-w-0 space-y-6'>
      {versioned && result.summary && (
        <section
          className='bg-muted/50 space-y-3 rounded-lg border p-4'
          aria-label={t('Report summary')}
        >
          <h3 className='flex items-center gap-2 font-semibold'>
            <Pin aria-hidden className='size-4' />
            {t('Report summary')}
            <SeverityBadge severity={result.severity} />
          </h3>
          <p className='text-sm leading-relaxed break-words whitespace-pre-line'>
            {result.summary}
          </p>
          {result.run && result.run.calls > 0 && (
            <p className='text-muted-foreground text-xs'>
              {t(
                'Model {{model}} · {{calls}} calls · {{tokens}} input tokens · {{seconds}} s',
                {
                  model: result.model,
                  calls: result.run.calls,
                  tokens: formatNumber(result.run.input_tokens, locale),
                  seconds: Math.round(result.run.duration_ms / 1000),
                }
              )}
              {(result.coverage?.reused_days ?? 0) > 0 &&
                ` · ${t('{{days}} days reused from earlier reports', {
                  days: result.coverage?.reused_days,
                })}`}
            </p>
          )}
        </section>
      )}
      {versioned && !props.task.payload.request_id && (
        <section
          className='min-w-0 space-y-3'
          aria-label={t('Period comparison')}
        >
          <h3 className='flex items-center gap-2 font-semibold'>
            <TrendingUp aria-hidden className='size-4' />
            {t('Period comparison')}
          </h3>
          {compared ? (
            <>
              <div className='text-muted-foreground space-y-1 text-xs'>
                <p>
                  {t('Current period')}:{' '}
                  {date.format(new Date(current.filter.start * 1000))} —{' '}
                  {date.format(new Date(current.filter.end * 1000))}
                </p>
                <p>
                  {t('Previous period')}:{' '}
                  {date.format(new Date(previous.filter.start * 1000))} —{' '}
                  {date.format(new Date(previous.filter.end * 1000))}
                </p>
                {result.timezone && (
                  <p>
                    {t('Timezone')}: {result.timezone}
                  </p>
                )}
              </div>
              <StaticDataTable
                data={categories}
                getRowKey={(row) => row.key}
                columns={[
                  {
                    id: 'category',
                    header: t('Category'),
                    cell: (row) => row.label,
                  },
                  {
                    id: 'current',
                    header: t('Current period'),
                    cell: (row) => formatNumber(current[row.key] ?? 0, locale),
                  },
                  {
                    id: 'previous',
                    header: t('Previous period'),
                    cell: (row) => formatNumber(previous[row.key] ?? 0, locale),
                  },
                  {
                    id: 'rate',
                    header: t('Request rate: previous → current'),
                    cell: (row) => {
                      const before =
                        previous.requests > 0
                          ? `${formatNumber(((previous[row.key] ?? 0) / previous.requests) * 100, locale, { maximumFractionDigits: 2 })}%`
                          : '—'
                      const after =
                        current.requests > 0
                          ? `${formatNumber(((current[row.key] ?? 0) / current.requests) * 100, locale, { maximumFractionDigits: 2 })}%`
                          : '—'
                      return `${before} → ${after}`
                    },
                  },
                ]}
              />
              <p className='text-muted-foreground text-xs'>
                {t(
                  'Rates use all recorded requests in each period. Categories can overlap; counts must not be added together.'
                )}
              </p>
            </>
          ) : (
            <p className='text-muted-foreground text-sm'>
              {t(
                'Previous-period data is unavailable or incomplete; changes cannot be compared.'
              )}
            </p>
          )}
          {current && current.timeline.length > 0 && (
            <Suspense fallback={<LoadingState />}>
              <TrendChart
                data={current.timeline}
                interval={result.timeline_interval}
                timezone={result.timezone}
              />
            </Suspense>
          )}
        </section>
      )}
      {versioned && result.sections && result.sections.length > 0 ? (
        <>
          {issues.length > 1 && (
            <div
              className='flex flex-wrap items-center gap-2'
              role='group'
              aria-label={t('Filter issues')}
            >
              {severities.length > 1 && (
                <NativeSelect
                  size='sm'
                  aria-label={t('Severity')}
                  value={severity}
                  onChange={(event) =>
                    setFilter({ ...filter, severity: event.target.value })
                  }
                >
                  <NativeSelectOption value=''>
                    {t('All severities')}
                  </NativeSelectOption>
                  {severities.map((value) => (
                    <NativeSelectOption key={value} value={value}>
                      {severityLabels[value]}
                    </NativeSelectOption>
                  ))}
                </NativeSelect>
              )}
              {channels.length > 1 && (
                <NativeSelect
                  size='sm'
                  aria-label={t('Channel')}
                  value={channel}
                  onChange={(event) =>
                    setFilter({ ...filter, channel: event.target.value })
                  }
                >
                  <NativeSelectOption value=''>
                    {t('All channels')}
                  </NativeSelectOption>
                  {channels.map((value) => (
                    <NativeSelectOption key={value} value={value}>
                      {t('Channel')} {value}
                    </NativeSelectOption>
                  ))}
                </NativeSelect>
              )}
              {models.length > 1 && (
                <NativeSelect
                  size='sm'
                  aria-label={t('Model')}
                  value={model}
                  onChange={(event) =>
                    setFilter({ ...filter, model: event.target.value })
                  }
                >
                  <NativeSelectOption value=''>
                    {t('All models')}
                  </NativeSelectOption>
                  {models.map((value) => (
                    <NativeSelectOption key={value} value={value}>
                      {value}
                    </NativeSelectOption>
                  ))}
                </NativeSelect>
              )}
              <span className='text-muted-foreground text-xs'>
                {t('{{shown}} of {{total}} issues', {
                  shown: visibleIssues,
                  total: issues.length,
                })}
              </span>
            </div>
          )}
          <nav
            className='flex flex-wrap gap-x-4 gap-y-2 text-sm'
            aria-label={t('Report sections')}
          >
            {visibleSections.map((section) => (
              <a
                key={section.id}
                href={`#analysis-section-${encodeURIComponent(section.id)}`}
                className='text-primary underline underline-offset-4'
              >
                {section.title}
              </a>
            ))}
          </nav>
          {visibleSections.map((section) => {
            const Icon = section.id.startsWith('issue-')
              ? Search
              : (sectionIcons[section.id as keyof typeof sectionIcons] ??
                FileText)
            return (
              <section
                key={section.id}
                id={`analysis-section-${encodeURIComponent(section.id)}`}
                className='min-w-0 scroll-mt-4 space-y-3 border-t pt-5'
              >
                <h3 className='flex items-center gap-2 font-semibold'>
                  {!/^\p{Extended_Pictographic}/u.test(section.title) && (
                    <Icon aria-hidden className='size-4 shrink-0' />
                  )}
                  {section.title}
                  <SeverityBadge severity={section.severity} />
                </h3>
                {section.id.startsWith('issue-') &&
                  (section.affected ?? 0) + (section.previous_affected ?? 0) >
                    0 && (
                    <p className='text-muted-foreground text-xs'>
                      {t('Affected requests')}:{' '}
                      {formatNumber(section.affected ?? 0, locale)}
                      {(section.previous_affected ?? 0) > 0 &&
                        ` (${t('Previous period')}: ${formatNumber(section.previous_affected ?? 0, locale)})`}
                      {(section.channels?.length ?? 0) > 0 &&
                        ` · ${t('Channels')}: ${section.channels?.join(', ')}`}
                      {(section.models?.length ?? 0) > 0 &&
                        ` · ${t('Models')}: ${section.models?.join(', ')}`}
                    </p>
                  )}
                <Suspense fallback={<LoadingState />}>
                  <Markdown blockRemoteContent className='min-w-0 break-words'>
                    {section.content}
                  </Markdown>
                </Suspense>
                {(section.evidence_ids?.length ?? 0) > 0 && (
                  <div
                    className='flex flex-wrap gap-2'
                    aria-label={t('Request diagnostics')}
                  >
                    {section.evidence_ids?.map((requestID) => (
                      <Button
                        key={requestID}
                        variant='outline'
                        size='sm'
                        className='h-auto max-w-full whitespace-normal'
                        onClick={() => setDetail({ requestID })}
                      >
                        <Paperclip aria-hidden className='shrink-0' />
                        <span className='break-all'>{requestID}</span>
                      </Button>
                    ))}
                  </div>
                )}
              </section>
            )
          })}
        </>
      ) : (
        <Suspense fallback={<LoadingState />}>
          <Markdown blockRemoteContent className='min-w-0 break-words'>
            {result?.report || props.task.state?.report || ''}
          </Markdown>
        </Suspense>
      )}
      {versioned && current && current.logs.length > 0 && (
        <details className='space-y-3 rounded-lg border p-3'>
          <summary className='cursor-pointer text-sm font-medium'>
            {t('Saved diagnostic samples')}
          </summary>
          <div className='flex flex-wrap gap-2'>
            {current.logs.map((row) => (
              <Button
                key={`${row.request_id}-${row.id}`}
                size='sm'
                variant='outline'
                onClick={() => setDetail({ row })}
                className='h-auto max-w-full whitespace-normal'
              >
                <Paperclip aria-hidden className='shrink-0' />
                <span className='break-all'>
                  {row.request_id ||
                    `${labels[row.categories[0]] ?? t('Unknown')} · ${formatTimestamp(row.created_at)}`}
                </span>
              </Button>
            ))}
          </div>
        </details>
      )}
      {detail && (
        <RequestEvidenceDialog
          row={detail.row}
          requestID={detail.requestID}
          taskID={props.task.task_id}
          onClose={() => setDetail(null)}
        />
      )}
    </div>
  )
}
