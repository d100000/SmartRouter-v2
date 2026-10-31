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
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Badge } from '@/components/ui/badge'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber, formatTimestamp } from '@/lib/format'

import { getAnalysis } from './api'
import { useAnalysisLabels } from './labels'
import type { FailureLog, UpstreamEvidence } from './types'

// JsonCodeEditor is an editable form control. Evidence can also be plain text,
// so use semantic preformatted content and the shared copy action for inspection.
function EvidenceBody(props: {
  title: string
  value: unknown
  incomplete?: boolean
}) {
  const { t } = useTranslation()
  const text =
    typeof props.value === 'string'
      ? props.value
      : JSON.stringify(props.value, null, 2)
  return (
    <section className='min-w-0 space-y-2'>
      <div className='flex items-center justify-between gap-2'>
        <h4 className='text-sm font-medium'>{props.title}</h4>
        {text && (
          <CopyButton
            value={text}
            size='sm'
            aria-label={t('Copy {{label}}', { label: props.title })}
          />
        )}
      </div>
      {props.incomplete && (
        <p role='status' className='text-warning text-xs'>
          {t('Evidence is incomplete or truncated.')}
        </p>
      )}
      {text ? (
        <pre
          tabIndex={0}
          className='bg-muted max-h-64 overflow-auto rounded-md p-3 font-mono text-xs break-words whitespace-pre-wrap'
        >
          {text}
        </pre>
      ) : (
        <p className='text-muted-foreground text-sm'>{t('Not recorded')}</p>
      )}
    </section>
  )
}

function RecordEvidence(props: { record: FailureLog }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const labels = useAnalysisLabels()
  const row = props.record
  const local = row.evidence?.local
  const down = row.evidence?.downstream
  const facts = [
    [
      t('Consumption log quota'),
      formatNumber(row.log_type === 2 ? (row.quota ?? 0) : 0, locale),
    ],
    [
      t('Funding settlement'),
      local ? (labels[local.state] ?? local.state) : t('Not recorded'),
    ],
    [
      t('Confirmed quota'),
      local?.state === 'confirmed'
        ? formatNumber(local.confirmed_quota, locale)
        : t('Unknown'),
    ],
    [
      t('Pre-consumed quota'),
      local ? formatNumber(local.pre_consumed_quota, locale) : t('Unknown'),
    ],
    [
      t('Input / output tokens'),
      row.log_type === 2
        ? `${formatNumber(row.prompt_tokens ?? 0, locale)} / ${formatNumber(row.completion_tokens, locale)}`
        : t('Not recorded'),
    ],
    [t('Usage source'), row.usage_source || t('Not recorded')],
    [
      t('Zero-output charge skipped'),
      row.no_charge == null ? t('Unknown') : t(row.no_charge ? 'Yes' : 'No'),
    ],
    [t('Downstream HTTP status'), down?.status_code || t('Not recorded')],
    [
      t('Completion signal written'),
      down && (down.completed || !down.capture_incomplete)
        ? t(down.completed ? 'Yes' : 'No')
        : t('Unknown'),
    ],
    [
      t('Downstream write failed'),
      down ? t(down.write_failed ? 'Yes' : 'No') : t('Unknown'),
    ],
    [
      t('Downstream error observed'),
      down ? t(down.failed ? 'Yes' : 'No') : t('Unknown'),
    ],
  ]
  return (
    <li className='min-w-0 space-y-4 border-t pt-4'>
      <div className='flex flex-wrap items-center gap-2 text-sm'>
        <strong>
          {row.log_type === 2 ? t('Consumption record') : t('Error record')}
        </strong>
        <span>
          {formatTimestamp(row.created_at)} · {t('Channel')}{' '}
          {formatNumber(row.channel, locale)}
        </span>
        {row.categories.map((kind) => (
          <Badge key={kind} variant='outline'>
            {labels[kind] ?? kind}
          </Badge>
        ))}
      </div>
      <p className='font-mono text-xs break-all'>
        {t('Upstream request ID')}:{' '}
        {row.upstream_request_id || t('Not recorded')}
      </p>
      <dl className='grid grid-cols-1 gap-3 text-sm sm:grid-cols-2 lg:grid-cols-3'>
        {facts.map(([label, value]) => (
          <div key={label} className='min-w-0'>
            <dt className='text-muted-foreground'>{label}</dt>
            <dd className='break-all'>{value}</dd>
          </div>
        ))}
      </dl>
      {local?.error && (
        <EvidenceBody title={t('Settlement error')} value={local.error} />
      )}
      {row.error_message && (
        <EvidenceBody
          title={t('Gateway error')}
          value={row.error_message}
          incomplete={row.error_truncated}
        />
      )}
      <EvidenceBody
        title={t('Downstream usage')}
        value={down?.usage}
        incomplete={down?.capture_incomplete}
      />
      <EvidenceBody title={t('Protocol diagnostics')} value={row.diagnostic} />
    </li>
  )
}

export function RequestEvidenceDialog(props: {
  row: FailureLog
  onClose: () => void
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const labels = useAnalysisLabels()
  const query = useQuery({
    queryKey: ['error-analysis', 'request', props.row.request_id],
    queryFn: ({ signal }) =>
      getAnalysis(
        { start: 0, end: 0, request_id: props.row.request_id },
        0,
        '',
        signal,
        true
      ),
    enabled: !!props.row.request_id,
    staleTime: 15000,
  })
  const detail = props.row.request_id ? query.data?.snapshot.logs[0] : props.row
  const records = detail?.records ?? (detail ? [detail] : [])
  // Newer snapshots contain the fullest version of an internal retry. Include
  // earlier log rows as well so failed attempts on other channels remain visible.
  const attempts = new Map<number, UpstreamEvidence>()
  for (const record of records) {
    for (const attempt of record.evidence?.upstream ?? []) {
      if (!attempts.has(attempt.attempt)) attempts.set(attempt.attempt, attempt)
    }
  }
  const upstream = [...attempts.values()].sort((a, b) => a.attempt - b.attempt)
  const incomplete =
    query.data?.snapshot.truncated ||
    detail?.records_truncated ||
    records.some((record) => record.evidence?.attempts_truncated)
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) props.onClose()
      }}
      title={t('Request diagnostics')}
      description={t(
        'Provider usage is not a billing receipt. Downstream delivery does not confirm downstream settlement.'
      )}
      contentClassName='sm:max-w-5xl'
      contentHeight='70vh'
      footer={
        detail && !query.isError ? (
          <CopyButton
            value={JSON.stringify(detail, null, 2)}
            size='sm'
            variant='outline'
          >
            {t('Copy diagnostics')}
          </CopyButton>
        ) : undefined
      }
    >
      {query.isPending && props.row.request_id && <LoadingState />}
      {query.isError && <ErrorState onRetry={() => void query.refetch()} />}
      {query.isSuccess && !detail && (
        <EmptyState title={t('Request ID not found')} />
      )}
      {detail && !query.isError && (
        <div className='min-w-0 space-y-6'>
          <div className='flex items-center gap-2'>
            <p className='min-w-0 font-mono text-xs break-all'>
              {detail.request_id || t('Unknown')}
            </p>
            {detail.request_id && (
              <CopyButton
                value={detail.request_id}
                aria-label={t('Copy request ID')}
              />
            )}
          </div>
          <div className='space-y-2 text-sm'>
            <div className='flex flex-wrap gap-2'>
              {detail.categories.map((kind) => (
                <Badge key={kind} variant='outline'>
                  {labels[kind] ?? kind}
                </Badge>
              ))}
            </div>
            <p>
              {t('Recorded consumption quota')}:{' '}
              <strong>
                {formatNumber(detail.recorded_quota ?? 0, locale)}
              </strong>{' '}
              · {t('{{count}} diagnostic records', { count: detail.attempts })}
            </p>
            <dl className='grid grid-cols-1 gap-3 border-y py-3 sm:grid-cols-2 lg:grid-cols-4'>
              <div>
                <dt className='text-muted-foreground'>{t('Upstream usage')}</dt>
                <dd>
                  {upstream.some(
                    (attempt) => Object.keys(attempt.usage ?? {}).length > 0
                  )
                    ? t('Yes')
                    : t('Not recorded')}
                </dd>
              </div>
              <div>
                <dt className='text-muted-foreground'>
                  {t('Funding settlement')}
                </dt>
                <dd>
                  {detail.evidence?.local
                    ? (labels[detail.evidence.local.state] ??
                      detail.evidence.local.state)
                    : t('Not recorded')}
                </dd>
              </div>
              <div>
                <dt className='text-muted-foreground'>
                  {t('Downstream usage')}
                </dt>
                <dd>
                  {Object.keys(detail.evidence?.downstream?.usage ?? {})
                    .length > 0
                    ? t('Yes')
                    : t('Not recorded')}
                </dd>
              </div>
              <div>
                <dt className='text-muted-foreground'>
                  {t('Completion signal written')}
                </dt>
                <dd>
                  {detail.evidence?.downstream?.completed
                    ? t('Yes')
                    : t('Not recorded')}
                </dd>
              </div>
            </dl>
            <p className='text-muted-foreground'>
              {t(
                'Delivery evidence is captured when the log is written; later events may be absent.'
              )}
            </p>
            <p className='text-muted-foreground'>
              {t(
                'Quota is shown in ledger units. Error records do not prove that a refund completed.'
              )}
            </p>
            <p className='text-muted-foreground'>
              {t(
                'Historical logs and unsupported transports may lack evidence. Missing data is unknown, not zero.'
              )}
            </p>
            {incomplete && (
              <p role='status' className='text-warning'>
                {t('Evidence is incomplete or truncated.')}
              </p>
            )}
          </div>
          <section className='min-w-0 space-y-4'>
            <h3 className='font-semibold'>{t('Upstream attempts')}</h3>
            {!upstream.length && (
              <p className='text-muted-foreground text-sm'>
                {t('Not recorded')}
              </p>
            )}
            {upstream.map((attempt) => (
              <section
                key={attempt.attempt}
                className='min-w-0 space-y-3 border-t pt-3'
              >
                <h4 className='text-sm font-medium'>
                  {t('Attempt {{current}}', { current: attempt.attempt })} ·{' '}
                  {t('Channel')} {formatNumber(attempt.channel, locale)} · HTTP{' '}
                  {attempt.status_code}
                </h4>
                <p className='font-mono text-xs break-all'>
                  {t('Upstream request ID')}:{' '}
                  {attempt.request_id || t('Not recorded')}
                </p>
                {attempt.capture_incomplete && (
                  <p role='status' className='text-warning text-xs'>
                    {t('Evidence is incomplete or truncated.')}
                  </p>
                )}
                <EvidenceBody
                  title={t('Upstream error body')}
                  value={attempt.error_body}
                  incomplete={attempt.error_truncated}
                />
                <EvidenceBody
                  title={t('Upstream usage')}
                  value={attempt.usage}
                />
              </section>
            ))}
          </section>
          <section className='min-w-0 space-y-4'>
            <h3 className='font-semibold'>
              {t('Local billing and downstream delivery')}
            </h3>
            <ol className='space-y-5'>
              {records.map((record) => (
                <RecordEvidence
                  key={`${record.id}:${record.created_at}:${record.log_type}:${record.channel}:${record.upstream_request_id}:${record.evidence?.upstream?.at(-1)?.attempt ?? 0}`}
                  record={record}
                />
              ))}
            </ol>
          </section>
        </div>
      )}
    </Dialog>
  )
}
