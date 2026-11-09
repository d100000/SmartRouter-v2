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
import { useQuery } from '@tanstack/react-query'
import { RotateCcw } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Progress } from '@/components/ui/progress'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber, formatTimestamp } from '@/lib/format'

import { getReports } from './api'
import { useAnalysisLabels } from './labels'
import { ReportContent } from './report-content'
import type { AnalysisFilter } from './types'
import { useAnalysisStream } from './use-analysis-stream'

export function AnalysisReports(props: {
  selected: string
  onSelect: (id: string) => void
  onRerun: (filter: AnalysisFilter) => void
  onResume: (id: string) => void
  pending: boolean
  configured: boolean
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const labels = useAnalysisLabels()
  const [offset, setOffset] = useState(0)
  const history = useQuery({
    meta: { errorRedirect: false },
    queryKey: ['error-analysis', 'reports', offset],
    queryFn: () => getReports(offset),
    refetchInterval: 10000,
  })
  const selected = props.selected || history.data?.items[0]?.task_id || ''
  const reportQuery = useAnalysisStream(selected)
  const report = reportQuery.data
  const busy = report?.status === 'running' || report?.status === 'pending'
  const content = report?.result?.report || report?.state?.report || ''
  const attempts = report?.result?.attempts || report?.state?.attempts || []
  const coverage =
    report?.state?.version === 2 ? report.state : report?.result?.coverage
  const versioned =
    report?.state?.version === 2 || report?.result?.version === 2
  const totalKnown =
    coverage?.total_known ??
    ((coverage?.total_rows ?? 0) > 0 || report?.state?.phase !== 'collecting')
  const anyActive = history.data?.items.some(
    (task) => task.status === 'running' || task.status === 'pending'
  )
  return (
    <div className='grid min-w-0 gap-6 lg:grid-cols-[16rem_minmax(0,1fr)]'>
      <aside className='min-w-0 space-y-3' aria-label={t('Analysis history')}>
        <h2 className='font-semibold'>{t('Analysis history')}</h2>
        {history.isPending && <LoadingState />}
        {history.isError && (
          <ErrorState onRetry={() => void history.refetch()} />
        )}
        {history.data?.items.length === 0 && (
          <EmptyState
            title={t('No analysis reports yet')}
            description={t('Configure a model and start your first analysis.')}
          />
        )}
        <div className='max-h-96 space-y-1 overflow-y-auto lg:max-h-[40rem]'>
          {history.data?.items.map((task) => (
            <Button
              key={task.task_id}
              variant={selected === task.task_id ? 'secondary' : 'ghost'}
              className='h-auto w-full justify-start py-3 text-left'
              onClick={() => props.onSelect(task.task_id)}
              aria-pressed={selected === task.task_id}
            >
              <span className='min-w-0 space-y-1'>
                <span className='block text-xs'>
                  {formatTimestamp(task.created_at)}
                </span>
                <span className='flex flex-wrap gap-2'>
                  <Badge
                    variant={
                      task.status === 'failed' ? 'destructive' : 'outline'
                    }
                  >
                    {labels[task.status]}
                  </Badge>
                </span>
                <span className='text-muted-foreground block text-xs whitespace-normal'>
                  {task.payload.request_id ? (
                    <span className='block break-all'>
                      {t('Request ID')}: {task.payload.request_id}
                    </span>
                  ) : (
                    <>
                      <span className='block'>
                        {formatTimestamp(task.payload.start)}
                      </span>
                      <span className='block'>
                        — {formatTimestamp(task.payload.end)}
                      </span>
                    </>
                  )}
                  <span className='block break-words'>
                    {task.payload.model || t('All models')} · {t('Channel')}{' '}
                    {task.payload.channel || t('All')}
                  </span>
                </span>
              </span>
            </Button>
          ))}
        </div>
        <div className='flex gap-2'>
          <Button
            size='sm'
            variant='outline'
            disabled={offset === 0}
            onClick={() => setOffset(Math.max(0, offset - 20))}
          >
            {t('Previous')}
          </Button>
          <Button
            size='sm'
            variant='outline'
            disabled={offset + 20 >= (history.data?.total ?? 0)}
            onClick={() => setOffset(offset + 20)}
          >
            {t('Next')}
          </Button>
        </div>
      </aside>
      <section
        className='min-w-0 space-y-4 lg:border-l lg:pl-6'
        aria-label={t('Analysis report')}
      >
        {selected && reportQuery.isPending && <LoadingState />}
        {reportQuery.isError && (
          <ErrorState
            description={t(
              'Unable to load the report. Background analysis continues.'
            )}
            onRetry={() => void reportQuery.refetch()}
          />
        )}
        {!selected && (
          <EmptyState
            title={t('AI error analysis')}
            description={t(
              'Reports explain probable causes, affected models and channels, and prioritized recovery actions.'
            )}
          />
        )}
        {report && (
          <>
            <div className='flex flex-wrap items-center justify-between gap-3'>
              <div className='space-y-1'>
                <h2 className='font-semibold'>{t('Analysis report')}</h2>
                <p className='text-muted-foreground text-xs break-all'>
                  {report.payload.request_id
                    ? `${t('Request ID')}: ${report.payload.request_id}`
                    : `${formatTimestamp(report.payload.start)} — ${formatTimestamp(report.payload.end)}`}
                </p>
                <p className='text-muted-foreground text-xs break-words'>
                  {t('Model')}: {report.payload.model || t('All models')} ·{' '}
                  {t('Channel ID')}: {report.payload.channel || t('All')}
                </p>
                {report.result?.model && (
                  <p className='text-muted-foreground text-xs break-words'>
                    {t('Analysis model')}: {report.result.model}
                  </p>
                )}
              </div>
              <div className='flex flex-wrap gap-2'>
                {content && (
                  <CopyButton value={content} size='sm' variant='outline'>
                    {t('Copy report')}
                  </CopyButton>
                )}
                {report.state?.can_resume && !busy && (
                  <Button
                    size='sm'
                    disabled={anyActive || props.pending || !props.configured}
                    onClick={() => props.onResume(report.task_id)}
                  >
                    <RotateCcw aria-hidden />
                    {t('Continue analysis')}
                  </Button>
                )}
                <Button
                  size='sm'
                  variant='outline'
                  disabled={
                    busy || anyActive || props.pending || !props.configured
                  }
                  onClick={() => props.onRerun(report.payload)}
                >
                  {t('Analyze again')}
                </Button>
              </div>
            </div>
            <div
              role='status'
              aria-live='polite'
              className='bg-muted space-y-2 rounded-lg p-3 text-sm'
            >
              <div className='flex flex-wrap items-center gap-2'>
                <Badge
                  variant={
                    report.status === 'failed' ? 'destructive' : 'outline'
                  }
                >
                  {!busy && versioned && report.result?.complete === false
                    ? t('Partial analysis')
                    : labels[report.status]}
                </Badge>
                {busy && (
                  <span>{labels[report.state?.phase ?? 'pending']}</span>
                )}
                {report.state?.attempt ? (
                  <span>
                    {t('Attempt {{current}} of {{total}}', {
                      current: report.state.attempt,
                      total: report.state.max_attempts,
                    })}
                  </span>
                ) : null}
              </div>
              {coverage && (
                <div className='space-y-2'>
                  <p>
                    {t('Log rows scanned: {{done}} / {{total}}', {
                      done: formatNumber(coverage.scanned_rows ?? 0, locale),
                      total: totalKnown
                        ? formatNumber(coverage.total_rows ?? 0, locale)
                        : t('Unknown'),
                    })}
                  </p>
                  {coverage.total_chunks != null && (
                    <p>
                      {t('Segments completed: {{done}} / {{total}}', {
                        done: formatNumber(
                          coverage.completed_chunks ?? 0,
                          locale
                        ),
                        total: formatNumber(coverage.total_chunks ?? 0, locale),
                      })}
                    </p>
                  )}
                  {(coverage.total_chunks ?? 0) > 0 && (
                    <Progress
                      aria-label={t('Segment progress')}
                      value={coverage.completed_chunks ?? 0}
                      max={coverage.total_chunks}
                    />
                  )}
                </div>
              )}
              {report.state?.current_step && (
                <p className='break-words'>{report.state.current_step}</p>
              )}
              {report.state?.max_calls != null && (
                <p className='text-muted-foreground text-xs'>
                  {t('Model calls: {{used}} / {{total}}', {
                    used: formatNumber(report.state.calls_used ?? 0, locale),
                    total: formatNumber(report.state.max_calls, locale),
                  })}
                </p>
              )}
              {busy && (
                <p>
                  {reportQuery.connection === 'polling'
                    ? t(
                        'Live connection interrupted. Progress is refreshed automatically; the background task continues.'
                      )
                    : t(
                        'Analysis continues in the background when you leave this page.'
                      )}
                </p>
              )}
              {report.status === 'pending' &&
                Date.now() / 1000 - report.created_at > 120 && (
                  <p role='alert'>
                    {t(
                      'Analysis is still queued. Check that a master node is running and can access the task database.'
                    )}
                  </p>
                )}
              {report.state?.next_retry_at && busy ? (
                <p>
                  {t('Next retry at {{time}}', {
                    time: formatTimestamp(report.state.next_retry_at),
                  })}
                </p>
              ) : null}
              <p className='text-muted-foreground text-xs'>
                {t('Updated at {{time}}', {
                  time: formatTimestamp(report.updated_at),
                })}
              </p>
            </div>
            {!busy &&
              (report.status === 'failed' ||
                (versioned && report.result?.complete === false)) && (
                <div
                  role='alert'
                  className='text-destructive space-y-2 text-sm'
                >
                  <p>
                    {report.state?.can_resume &&
                    report.error === 'task lease expired'
                      ? t(
                          'The worker stopped responding. Continue from the saved progress.'
                        )
                      : labels[report.error] || report.error}
                  </p>
                  <p>
                    {t(
                      versioned
                        ? 'This report covers only completed analysis. Unfinished segments are listed in the progress above.'
                        : 'Any text below is an incomplete draft. A new analysis creates a separate history entry.'
                    )}
                  </p>
                </div>
              )}
            {(report.result?.coverage?.warnings?.length ?? 0) > 0 && (
              <div
                role='status'
                className='bg-muted space-y-1 rounded-lg p-3 text-sm'
              >
                {report.result?.coverage?.warnings?.map((warning) => (
                  <p key={warning}>
                    {labels[warning] ??
                      t(
                        'Some analysis data is incomplete. Review coverage before drawing conclusions.'
                      )}
                  </p>
                ))}
              </div>
            )}
            {content ||
            report.result?.summary ||
            (report.result?.sections?.length ?? 0) > 0 ? (
              <ReportContent key={report.task_id} task={report} />
            ) : (
              <EmptyState
                title={
                  busy ? t('Waiting for model output') : t('No report content')
                }
              />
            )}
            {attempts.length > 0 && (
              <details
                className='rounded-lg border p-3 text-sm'
                open={report.status === 'failed'}
              >
                <summary className='cursor-pointer font-medium'>
                  {t('Model execution diagnostics')}
                </summary>
                <ol className='mt-3 space-y-3'>
                  {attempts.map((attempt) => (
                    <li key={attempt.number}>
                      <strong>
                        {t('Attempt {{current}}', { current: attempt.number })}
                      </strong>{' '}
                      · {formatTimestamp(attempt.started_at)}
                      <p>
                        {labels[attempt.code] || attempt.code}
                        {attempt.status_code > 0
                          ? ` · HTTP ${attempt.status_code}`
                          : ''}
                      </p>
                      <div className='text-muted-foreground mt-1 flex flex-wrap gap-x-3 gap-y-1 text-xs'>
                        {attempt.duration_ms != null && (
                          <span>
                            {t('Elapsed: {{seconds}} seconds', {
                              seconds: formatNumber(
                                attempt.duration_ms / 1000,
                                locale,
                                { maximumFractionDigits: 3 }
                              ),
                            })}
                          </span>
                        )}
                        {attempt.timeout_seconds != null && (
                          <span>
                            {t('Timeout: {{seconds}} seconds', {
                              seconds: formatNumber(
                                attempt.timeout_seconds,
                                locale
                              ),
                            })}
                          </span>
                        )}
                        {attempt.idle_seconds != null && (
                          <span>
                            {t('No-token timeout: {{seconds}} seconds', {
                              seconds: formatNumber(
                                attempt.idle_seconds,
                                locale
                              ),
                            })}
                          </span>
                        )}
                        {attempt.max_output_tokens != null && (
                          <span>
                            {t('Output limit: {{tokens}} tokens', {
                              tokens: formatNumber(
                                attempt.max_output_tokens,
                                locale
                              ),
                            })}
                          </span>
                        )}
                        {attempt.finish_reason && (
                          <span>
                            {t('Finish reason: {{reason}}', {
                              reason:
                                labels[`finish_${attempt.finish_reason}`] ||
                                t('Unknown'),
                            })}
                          </span>
                        )}
                      </div>
                    </li>
                  ))}
                </ol>
              </details>
            )}
            {report.result?.snapshot && (
              <div className='space-y-2'>
                <p className='text-muted-foreground text-xs'>
                  {t(
                    '{{requests}} recorded requests; {{samples}} diagnostic samples sent to the model.',
                    {
                      requests: report.result.snapshot.requests,
                      samples: report.result.sample_count,
                    }
                  )}
                  {report.result.snapshot.truncated &&
                    ` ${t('Statistics cover a limited scan; review the report coverage notes.')}`}
                </p>
                {report.result.snapshot.log_recording_enabled === false && (
                  <p role='status' className='bg-muted rounded-lg p-3 text-sm'>
                    {t(
                      'Consumption logging is disabled. Counts and failure rates do not cover all requests.'
                    )}
                  </p>
                )}
                {report.result.snapshot.error_recording_enabled === false && (
                  <p role='status' className='bg-muted rounded-lg p-3 text-sm'>
                    {t(
                      'Request error logging is disabled. Enable ERROR_LOG_ENABLED on gateway nodes to include ordinary request failures.'
                    )}
                  </p>
                )}
              </div>
            )}
            {report.result?.snapshot_omitted && (
              <p role='status' className='bg-muted rounded-lg p-3 text-sm'>
                {t(
                  'Diagnostic samples could not be saved. The analysis report was preserved.'
                )}
              </p>
            )}
            <p className='text-muted-foreground text-xs'>
              {t(
                'AI conclusions are hypotheses. Verify the cited evidence before changing production configuration.'
              )}
            </p>
          </>
        )}
      </section>
    </div>
  )
}
