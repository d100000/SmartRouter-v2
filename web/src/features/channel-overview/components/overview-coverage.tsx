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
import { useTranslation } from 'react-i18next'

import { Badge } from '@/components/ui/badge'
import { toIntlLocale } from '@/i18n/languages'
import { formatBillingCurrencyFromUSD } from '@/lib/currency'
import { formatNumber, formatTimestamp } from '@/lib/format'

import type { OverviewSnapshot } from '../types'
import { OverviewHelp } from './overview-help'

export function OverviewCoverage(props: { snapshot: OverviewSnapshot }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const { summary, partial, collected_since, refreshed_at, warnings } =
    props.snapshot
  const unknownCount = formatNumber(summary.unknown_cost_attempts, locale)
  const unconfirmedCount = formatNumber(
    summary.unconfirmed_revenue_requests,
    locale
  )
  const subscriptionAmount = formatBillingCurrencyFromUSD(
    summary.subscription_revenue,
    {
      locale,
      abbreviate: false,
      digitsSmall: 8,
    }
  )
  return (
    <div className='text-muted-foreground flex min-w-0 flex-wrap items-center justify-between gap-x-4 gap-y-2 text-xs'>
      <div className='flex min-w-0 flex-wrap items-center gap-x-3 gap-y-2'>
        {summary.unknown_cost_attempts > 0 && (
          <Badge variant='warning'>
            {t('Unknown-cost attempts: {{count}}', { count: unknownCount })}
          </Badge>
        )}
        {summary.unconfirmed_revenue_requests > 0 && (
          <Badge variant='warning'>
            {t('Funding-unconfirmed requests: {{count}}', {
              count: unconfirmedCount,
            })}
          </Badge>
        )}
        {partial && <Badge variant='warning'>{t('Partial Data')}</Badge>}
        {summary.subscription_revenue > 0 && (
          <span>
            {t('Subscription usage value: {{amount}}', {
              amount: subscriptionAmount,
            })}
          </span>
        )}
        <span className='inline-flex items-center gap-1'>
          {t('Data Coverage')}
          <OverviewHelp title={t('Data Coverage')}>
            <p>
              {t(
                'Synchronous relay calls only. Asynchronous tasks are not included.'
              )}{' '}
              {t('Daily buckets use UTC.')}
            </p>
            <p>
              {t(
                'Unknown-cost attempts: {{count}}. Unknown cost is never treated as zero.',
                { count: unknownCount }
              )}
            </p>
            <p>
              {t(
                'Profit is shown only for requests with complete cost records.'
              )}
            </p>
            <p>
              {t(
                'Funding-unconfirmed requests: {{count}}. They are excluded from profit.',
                { count: unconfirmedCount }
              )}
            </p>
            <p>
              {t(
                'Subscription usage value: {{amount}}. This is a quota equivalent, not cash revenue.',
                { amount: subscriptionAmount }
              )}
            </p>
            <p>
              {t(
                'Data before collection started is unavailable. Historical calls are not recalculated.'
              )}
            </p>
            {collected_since != null && (
              <p>
                {t('Collection started at {{time}}', {
                  time: formatTimestamp(collected_since),
                })}
              </p>
            )}
            {warnings.length > 0 && (
              <ul className='list-disc pl-4'>
                {warnings.map((warning) => (
                  <li key={warning}>{t(warning)}</li>
                ))}
              </ul>
            )}
          </OverviewHelp>
        </span>
      </div>
      <span>
        {t('Updated at {{time}}', { time: formatTimestamp(refreshed_at) })}
      </span>
    </div>
  )
}
