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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { isAxiosError } from 'axios'
import { RefreshCw, Settings2 } from 'lucide-react'
import { lazy, Suspense, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { SectionPageLayout } from '@/components/layout'
import { LoadingState } from '@/components/loading-state'
import { GroupSelector, ModelSelector } from '@/components/model-group-selector'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { toIntlLocale } from '@/i18n/languages'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { formatNumber, formatTimestamp } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import {
  getScheduling,
  recoverSchedulingChannel,
  saveSchedulingConfig,
} from './api'
import { ChannelTable } from './components/channel-table'
import { ConfigDialog } from './components/config-dialog'
import { HealthOverview } from './components/health-overview'
import type { SchedulingChannel, SchedulingConfig } from './types'

const TrendCharts = lazy(() => import('./components/trend-charts'))

export function Scheduling() {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const queryClient = useQueryClient()
  const user = useAuthStore((state) => state.auth.user)
  const canRead = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.READ
  )
  const canWrite = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.WRITE
  )
  const canOperate = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.OPERATE
  )
  const [filter, setFilter] = useState({ group: '', model: '', stream: true })
  const [configure, setConfigure] = useState(false)
  const [recover, setRecover] = useState<SchedulingChannel | null>(null)
  const query = useQuery({
    queryKey: ['scheduling', filter],
    queryFn: ({ signal }) => getScheduling(filter, signal),
    refetchInterval: 15000,
    retry: false,
    enabled: canRead,
  })
  const data = query.data
  const phases = {
    cold: t('Cold start'),
    transition: t('Routing transition'),
    dynamic: t('Mature dynamic routing'),
  }
  useEffect(() => {
    // Resolve defaults once, after a fresh response with a configured route.
    // Polls and cached defaults must not replace an explicit selection.
    if (!query.isSuccess || query.isFetching || !data?.channels.length) return
    setFilter((current) => {
      if (current.group && current.group !== data.group) return current
      const group = current.group || data.group
      const model = current.model || data.model
      if (group === current.group && model === current.model) return current
      return { ...current, group, model }
    })
  }, [data, query.isSuccess, query.isFetching])
  const saveMutation = useMutation({
    mutationFn: (input: {
      group: string
      model: string
      config: SchedulingConfig
    }) => saveSchedulingConfig(input.group, input.model, input.config),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['scheduling'] })
      setConfigure(false)
      toast.success(t('Routing configuration saved'))
    },
  })
  const recoverMutation = useMutation({
    mutationFn: (input: { group: string; model: string; channelId: number }) =>
      recoverSchedulingChannel(input.group, input.model, input.channelId),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['scheduling'] })
      setRecover(null)
      toast.success(t('Channel weight recalculated'))
    },
  })
  return (
    <SectionPageLayout stackActionsOnMobile>
      <SectionPageLayout.Title>
        {t('Scheduling Center')}
      </SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        <Button
          variant='outline'
          size='sm'
          onClick={() => void query.refetch()}
          disabled={query.isFetching || !canRead}
        >
          <RefreshCw
            aria-hidden='true'
            className={query.isFetching ? 'animate-spin' : ''}
          />
          {t('Refresh')}
        </Button>
        {canWrite && (
          <Button
            variant='outline'
            size='sm'
            onClick={() => {
              saveMutation.reset()
              setConfigure(true)
            }}
            disabled={!data?.model || query.isError}
          >
            <Settings2 aria-hidden='true' />
            {t('Routing configuration')}
          </Button>
        )}
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <div className='min-w-0 space-y-4'>
          <p className='text-muted-foreground text-sm'>
            {t(
              'Monitor channel health and route each model request with live performance and capacity signals.'
            )}
          </p>
          {!canRead && (
            <ErrorState
              title={t('Access Forbidden')}
              description={t(
                'You do not have permission to perform this action.'
              )}
            />
          )}
          {query.isPending && canRead && <LoadingState />}
          {query.isError && (
            <ErrorState
              description={t(
                'Unable to load routing health. Retry to get the latest channel state.'
              )}
              onRetry={() => {
                if (
                  (filter.group || filter.model) &&
                  isAxiosError<{ code?: string }>(query.error) &&
                  query.error.response?.status === 400 &&
                  query.error.response.data?.code ===
                    'scheduling_selection_unavailable'
                ) {
                  setFilter({ group: '', model: '', stream: filter.stream })
                  return
                }
                void query.refetch()
              }}
            />
          )}
          {data && canRead && !query.isError && (
            <>
              <div className='grid min-w-0 grid-cols-1 items-center gap-3 rounded-lg border p-3 sm:flex sm:flex-wrap'>
                <GroupSelector
                  showLabelOnMobile
                  className='w-full max-w-full min-w-0 sm:w-auto'
                  selectedGroup={data.group}
                  groups={data.groups.map((group) => ({
                    value: group,
                    label: group,
                  }))}
                  onGroupChange={(group) =>
                    setFilter({ ...filter, group, model: '' })
                  }
                />
                <ModelSelector
                  showLabelOnMobile
                  className='w-full max-w-full min-w-0 sm:w-auto'
                  selectedModel={data.model}
                  models={data.models.map((model) => ({
                    value: model,
                    label: model,
                  }))}
                  onModelChange={(model) =>
                    setFilter({ ...filter, group: data.group, model })
                  }
                />
                <div className='flex min-w-0 items-center gap-2 sm:border-l sm:pl-3'>
                  <Switch
                    id='scheduling-stream'
                    checked={filter.stream}
                    onCheckedChange={(stream) =>
                      setFilter({
                        ...filter,
                        group: data.group,
                        model: data.model,
                        stream,
                      })
                    }
                  />
                  <Label htmlFor='scheduling-stream'>
                    {filter.stream ? t('Streaming') : t('Non-streaming')}
                  </Label>
                </div>
                <span className='text-muted-foreground text-xs break-words sm:ml-auto'>
                  {t('Updated at {{time}}', {
                    time: formatTimestamp(data.refreshed_at),
                  })}
                </span>
              </div>
              {data.groups.length === 0 || !data.model ? (
                <EmptyState
                  title={t('No channel routes configured')}
                  description={t(
                    'Add channels with groups and models to start collecting routing health.'
                  )}
                />
              ) : (
                <>
                  <div className='flex flex-wrap items-center gap-3 rounded-lg border p-3 text-xs'>
                    <Badge
                      variant={
                        data.active && data.config.enabled
                          ? 'default'
                          : 'outline'
                      }
                    >
                      {data.active && data.config.enabled
                        ? t('Intelligent routing active')
                        : t('Native routing')}
                    </Badge>
                    {data.config.enabled && (
                      <Badge variant='outline'>{phases[data.phase]}</Badge>
                    )}
                    {!data.active && data.config.enabled && (
                      <span>
                        {t(
                          'Activation progress: {{count}} / {{threshold}} original requests',
                          {
                            count: formatNumber(
                              data.activation_requests,
                              locale
                            ),
                            threshold: formatNumber(
                              data.activation_threshold,
                              locale
                            ),
                          }
                        )}
                      </span>
                    )}
                    <span className='text-muted-foreground'>
                      {t(
                        'Scores refresh every minute; errors and capacity update in real time.'
                      )}
                    </span>
                    {!data.active && data.config.enabled && (
                      <p className='text-muted-foreground basis-full leading-relaxed'>
                        {t(
                          'Cold start follows native priority and configured weights. Dynamic routing activates after 50 original requests in 30 minutes, then gradually replaces the initial weights.'
                        )}
                      </p>
                    )}
                  </div>
                  <Tabs defaultValue='overview' className='gap-4'>
                    <TabsList variant='line'>
                      <TabsTrigger value='overview'>
                        {t('Health overview')}
                      </TabsTrigger>
                      <TabsTrigger value='groups'>{t('Groups')}</TabsTrigger>
                    </TabsList>
                    <TabsContent value='overview' className='space-y-5'>
                      <HealthOverview data={data} />
                      <Suspense fallback={<LoadingState />}>
                        <TrendCharts points={data.trend} />
                      </Suspense>
                    </TabsContent>
                    <TabsContent value='groups' className='min-w-0 space-y-4'>
                      <div className='flex flex-wrap items-baseline justify-between gap-2'>
                        <h3 className='font-semibold'>
                          {t('Channel routing details')}
                        </h3>
                        <span className='text-muted-foreground text-xs'>
                          {data.group} / {data.model} ·{' '}
                          {t('{{count}} channels', {
                            count: data.channels.length,
                          })}
                        </span>
                      </div>
                      {data.channels.length > 0 ? (
                        <ChannelTable
                          channels={data.channels}
                          onRecover={(channel) => {
                            recoverMutation.reset()
                            setRecover(channel)
                          }}
                          pending={recoverMutation.isPending}
                          canOperate={canOperate}
                        />
                      ) : (
                        <EmptyState title={t('No channels for this model')} />
                      )}
                      <div className='text-muted-foreground grid gap-2 text-xs leading-relaxed lg:grid-cols-2'>
                        <p>
                          {t(
                            'Traffic share is observed dispatches over 30 minutes. Predicted routing probability is the current first-attempt distribution; retries use descending effective weight.'
                          )}
                        </p>
                        <p>
                          {t(
                            'Health attainment includes channel failures and successes with measured first output; only successful attempts within the latency target meet the goal.'
                          )}
                        </p>
                        <p>
                          {t(
                            'Health score reflects smoothed success and recent errors. Routing quality combines latency and health before live load adjustment.'
                          )}
                        </p>
                        <p>
                          {t(
                            'Streaming and non-streaming latency are measured separately. Heartbeats, empty chunks and role declarations do not count as first output.'
                          )}
                        </p>
                        <p>
                          {t(
                            'Initial configured weights apply during cold start and transition. Mature routing uses observed performance.'
                          )}
                        </p>
                        <p>
                          {t(
                            'Gradual rollout follows successful attempts and ends after 20 successes. Traffic is limited only while a healthy, learned alternative is available.'
                          )}
                        </p>
                      </div>
                    </TabsContent>
                  </Tabs>
                </>
              )}
              <div className='text-muted-foreground border-t pt-3 text-xs leading-relaxed'>
                <p>
                  {t(
                    'Current instance only. Upstream concurrency is shared across groups; restarting starts a new learning cycle.'
                  )}
                </p>
                <p>
                  {t('Collection started: {{time}}', {
                    time: formatTimestamp(data.started_at),
                  })}
                </p>
              </div>
              {configure && canWrite && (
                <ConfigDialog
                  key={`${data.group}/${data.model}`}
                  data={data}
                  pending={saveMutation.isPending}
                  error={saveMutation.isError}
                  onClose={() => setConfigure(false)}
                  onSave={(config) =>
                    saveMutation.mutate({
                      group: data.group,
                      model: data.model,
                      config,
                    })
                  }
                />
              )}
              <ConfirmDialog
                open={recover != null && canOperate}
                onOpenChange={(open) => {
                  if (!open && !recoverMutation.isPending) setRecover(null)
                }}
                title={t('Remove downweighting')}
                desc={t(
                  'Remove the recovery limit for {{channel}} and recalculate from current performance. Failure history, disabled status, cooldowns and capacity limits remain in effect. Poor recent performance can still keep the weight low.',
                  { channel: recover?.name ?? '' }
                )}
                confirmText={t('Recalculate weight')}
                isLoading={recoverMutation.isPending}
                handleConfirm={() => {
                  if (recover) {
                    recoverMutation.mutate({
                      group: data.group,
                      model: data.model,
                      channelId: recover.channel_id,
                    })
                  }
                }}
              >
                {recoverMutation.isError && (
                  <p role='alert' className='text-destructive text-sm'>
                    {t('Weight recalculation failed. Please try again.')}
                  </p>
                )}
              </ConfirmDialog>
            </>
          )}
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
