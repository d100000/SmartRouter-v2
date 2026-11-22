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
import { formatNumber, formatTimestamp } from '@/lib/format'

import type { SchedulingChannel } from '../types'

// CooldownDetails shows the channel's current or most recent cooldown cycle:
// when it started, its length, the time left, the requests that bypassed it
// and the recovery probes admitted after it expired. The remaining time is
// measured against the snapshot time so client clock skew cannot shift it.
export function CooldownDetails(props: {
  channel: SchedulingChannel
  refreshedAt: number
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const channel = props.channel
  if (!channel.cooldown_started_at) return null
  const cooling = channel.route_state === 'cooling'
  const until = channel.cooldown_until
    ? Math.floor(Date.parse(channel.cooldown_until) / 1000)
    : null
  const remaining =
    cooling && until != null ? Math.max(0, until - props.refreshedAt) : null
  const probeResult = {
    recovered: t('Recovered'),
    failed: t('Failed'),
  }
  return (
    <div
      className='text-muted-foreground mt-1 space-y-0.5 whitespace-nowrap'
      aria-label={t('Cooldown details')}
    >
      {cooling && channel.cooldown_channel_wide && (
        <Badge variant='destructive' className='block w-fit'>
          {t('Channel-wide cooldown')}
        </Badge>
      )}
      <span className='block'>
        {t('Cooldown started: {{time}}', {
          time: formatTimestamp(Date.parse(channel.cooldown_started_at) / 1000),
        })}
      </span>
      <span className='block'>
        {t('Cooldown length: {{seconds}} s (level {{level}})', {
          seconds: formatNumber(channel.cooldown_seconds ?? 0, locale),
          level: channel.cooldown_level ?? 0,
        })}
      </span>
      {remaining != null && (
        <span className='block'>
          {t('Time left: {{seconds}} s', {
            seconds: formatNumber(remaining, locale),
          })}
        </span>
      )}
      <span className='block'>
        {t('Requests skipped during cooldown: {{count}}', {
          count: channel.cooldown_skipped,
        })}
      </span>
      {channel.cooldown_probes > 0 && (
        <span className='block'>
          {t('Recovery probes: {{count}}, last result: {{result}}', {
            count: channel.cooldown_probes,
            result: channel.cooldown_probe_result
              ? probeResult[channel.cooldown_probe_result]
              : '—',
          })}
        </span>
      )}
    </div>
  )
}
