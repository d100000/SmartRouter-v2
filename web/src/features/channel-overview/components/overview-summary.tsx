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
  Activity,
  ChartNoAxesCombined,
  DollarSign,
  HeartPulse,
  Percent,
  Wallet,
} from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { StatCard } from '@/features/dashboard/components/ui/stat-card'
import { toIntlLocale } from '@/i18n/languages'
import { formatBillingCurrencyFromUSD } from '@/lib/currency'
import { formatNumber } from '@/lib/format'

import type { OverviewMetrics } from '../types'
import { OverviewHelp } from './overview-help'

export function OverviewSummary(props: { metrics: OverviewMetrics }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const currencyOptions = {
    locale,
    abbreviate: false,
    digitsLarge: 2,
    digitsSmall: 8,
  }
  const profitDescription =
    props.metrics.complete_attempts === 0
      ? t('No requests with complete cost records yet.')
      : t('Profit is shown only for requests with complete cost records.')
  const cards = [
    {
      title: t('Requests'),
      value: formatNumber(props.metrics.requests, locale),
      icon: Activity,
      description: t('Attempts: {{count}}', {
        count: formatNumber(props.metrics.attempts, locale),
      }),
    },
    {
      title: t('Billing Revenue'),
      value: formatBillingCurrencyFromUSD(
        props.metrics.revenue,
        currencyOptions
      ),
      icon: DollarSign,
      help: t('Settled usage value, not cash receipts.'),
    },
    {
      title: t('Known Upstream Cost'),
      value:
        props.metrics.cost_coverage == null
          ? '—'
          : formatBillingCurrencyFromUSD(props.metrics.cost, currencyOptions),
      icon: Wallet,
      description: t('Cost coverage: {{value}}', {
        value:
          props.metrics.cost_coverage == null
            ? '—'
            : `${formatNumber(props.metrics.cost_coverage, locale)}%`,
      }),
    },
    {
      title: t('Estimated Gross Profit'),
      value:
        props.metrics.complete_attempts === 0
          ? '—'
          : formatBillingCurrencyFromUSD(props.metrics.margin, currencyOptions),
      icon: ChartNoAxesCombined,
      help: profitDescription,
    },
    {
      title: t('Gross Margin'),
      value:
        props.metrics.margin_rate == null
          ? '—'
          : `${formatNumber(props.metrics.margin_rate, locale)}%`,
      icon: Percent,
      help: profitDescription,
    },
    {
      title: t('Attempt Success Rate'),
      value:
        props.metrics.health == null
          ? '—'
          : `${formatNumber(props.metrics.health, locale)}%`,
      icon: HeartPulse,
      help: t('Completed upstream attempts; retries are counted separately.'),
    },
  ]
  return (
    <section
      aria-label={t('Overview Metrics')}
      className='grid min-w-0 grid-cols-2 gap-3 lg:grid-cols-3 xl:grid-cols-6'
    >
      {cards.map((card) => (
        <div key={card.title} className='min-w-0 rounded-lg border p-3'>
          <StatCard
            {...card}
            density='compact'
            action={
              card.help ? (
                <OverviewHelp title={card.title}>{card.help}</OverviewHelp>
              ) : undefined
            }
          />
        </div>
      ))}
    </section>
  )
}
