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

import { StaticDataTable } from '@/components/data-table'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber, formatTimestamp } from '@/lib/format'
import { cn } from '@/lib/utils'

import type { SchedulingChannel } from '../types'

export function ChannelTable(props: {
  channels: SchedulingChannel[]
  onRecover: (channel: SchedulingChannel) => void
  pending: boolean
  canOperate: boolean
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const weightFormatter = new Intl.NumberFormat(locale, {
    maximumSignificantDigits: 3,
  })
  const number = (value: number | null) =>
    value == null ? '—' : formatNumber(value, locale)
  const percent = (value: number | null) =>
    value == null ? '—' : `${formatNumber(value * 100, locale)}%`
  const states = {
    eligible: t('Routing eligible'),
    ineligible: t('Unavailable'),
    degraded: t('Downweighted'),
    disabled: t('Disabled'),
    cooling: t('Cooling down'),
    saturated: t('At capacity'),
    cold: t('Cold start'),
  }
  return (
    <StaticDataTable
      tableClassName='min-w-[1800px] text-xs'
      containerProps={{ 'aria-label': t('Channel routing details') }}
    >
      <TableHeader>
        <TableRow className='bg-muted/50'>
          <TableHead colSpan={3}>{t('Routing and channel')}</TableHead>
          <TableHead colSpan={4}>{t('Traffic and weights')}</TableHead>
          <TableHead colSpan={3}>{t('Reliability and load')}</TableHead>
          <TableHead colSpan={5}>{t('Health and routing quality')}</TableHead>
          <TableHead
            rowSpan={2}
            className='bg-muted sticky right-0 z-20 text-right'
          >
            {t('Actions')}
          </TableHead>
        </TableRow>
        <TableRow>
          {[
            t('Routing state'),
            t('Channel Name'),
            t('Channel Status'),
            t('Traffic share (30m)'),
            t('Dynamic weight'),
            t('Initial configured weight'),
            t('Dispatches (30m)'),
            t('Success rate (30m)'),
            t('Success rate (5m)'),
            t('In-flight requests'),
            t('Health target attainment'),
            t('Health score'),
            t('Average time to first output (5m)'),
            t('Routing quality score'),
            t('Predicted routing probability'),
          ].map((label, index) => (
            <TableHead
              key={label}
              className={cn(
                'whitespace-nowrap',
                index === 1 && 'bg-background sticky left-0 z-10'
              )}
            >
              {label}
            </TableHead>
          ))}
        </TableRow>
      </TableHeader>
      <TableBody>
        {props.channels.map((channel) => (
          <TableRow key={channel.channel_id}>
            <TableCell>
              <Badge
                variant={
                  channel.route_state === 'degraded' ? 'destructive' : 'outline'
                }
              >
                {states[channel.route_state]}
              </Badge>
              {channel.ramp_limited && (
                <Badge variant='secondary' className='mt-1 block'>
                  {t('Gradual rollout: {{progress}}', {
                    progress: percent(channel.ramp_progress),
                  })}
                </Badge>
              )}
              {channel.cooldown_until && (
                <span className='text-muted-foreground mt-1 block whitespace-nowrap'>
                  {t('Cooldown ends: {{time}}', {
                    time: formatTimestamp(
                      Date.parse(channel.cooldown_until) / 1000
                    ),
                  })}
                </span>
              )}
            </TableCell>
            <TableCell className='bg-background sticky left-0 z-10 max-w-64 min-w-40'>
              <div className='truncate font-medium' title={channel.name}>
                {channel.name}
              </div>
              <span className='text-muted-foreground'>
                #{channel.channel_id}
              </span>
            </TableCell>
            <TableCell>
              {channel.status === 1 ? t('Enabled') : t('Disabled')}
            </TableCell>
            <TableCell className='tabular-nums'>
              {percent(channel.traffic_share)}
            </TableCell>
            <TableCell className='tabular-nums'>
              {weightFormatter.format(channel.effective_weight)}
            </TableCell>
            <TableCell className='tabular-nums'>
              {number(channel.configured_weight)}
              <span className='text-muted-foreground block'>
                {t('Priority')}: {number(channel.priority)}
              </span>
            </TableCell>
            <TableCell className='tabular-nums'>
              {number(channel.dispatches_30m)}
            </TableCell>
            <TableCell className='tabular-nums'>
              {percent(channel.success_rate_30m)}
            </TableCell>
            <TableCell className='tabular-nums'>
              {percent(channel.success_rate_5m)}
            </TableCell>
            <TableCell className='tabular-nums'>
              {number(channel.in_flight)} / {number(channel.capacity)}
            </TableCell>
            <TableCell className='tabular-nums'>
              {percent(channel.health_attainment)}
            </TableCell>
            <TableCell className='tabular-nums'>
              <TooltipProvider>
                <Tooltip>
                  <TooltipTrigger
                    render={
                      <Button
                        variant='ghost'
                        size='sm'
                        className='h-auto p-0 font-normal tabular-nums'
                      />
                    }
                    aria-label={t('Health details for {{channel}}', {
                      channel: channel.name,
                    })}
                    aria-describedby={`scheduling-health-${channel.channel_id}`}
                  >
                    {number(channel.health_score)}
                  </TooltipTrigger>
                  <TooltipContent
                    id={`scheduling-health-${channel.channel_id}`}
                    role='tooltip'
                  >
                    {t(
                      'Window baseline: {{baseline}}%. Recovery limit: {{limit}}%.',
                      {
                        baseline: number(channel.health_baseline),
                        limit: number(channel.recovery_limit),
                      }
                    )}
                  </TooltipContent>
                </Tooltip>
              </TooltipProvider>
            </TableCell>
            <TableCell className='tabular-nums'>
              {channel.avg_ttft_ms_5m == null
                ? '—'
                : t('{{value}} ms', {
                    value: number(channel.avg_ttft_ms_5m),
                  })}
            </TableCell>
            <TableCell className='tabular-nums'>
              {number(channel.quality_score)}
            </TableCell>
            <TableCell className='tabular-nums'>
              {percent(channel.selection_probability)}
            </TableCell>
            <TableCell className='bg-background sticky right-0 z-10 text-right'>
              {props.canOperate &&
              channel.can_recover &&
              channel.status === 1 ? (
                <Button
                  variant='outline'
                  size='sm'
                  disabled={props.pending}
                  onClick={() => props.onRecover(channel)}
                  aria-label={t('Remove downweighting for {{channel}}', {
                    channel: channel.name,
                  })}
                >
                  {t('Remove downweighting')}
                </Button>
              ) : (
                <span className='text-muted-foreground'>—</span>
              )}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </StaticDataTable>
  )
}
