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
import { Play, RefreshCw, Settings2 } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ErrorState } from '@/components/error-state'
import { SectionPageLayout } from '@/components/layout'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'

import { getAnalysis, getConfig, saveConfig, startAnalysis } from './api'
import { ConfigDialog } from './config-dialog'
import { FailureDashboard } from './dashboard'
import { AnalysisReports } from './reports'
import type { AnalysisFilter, ConfigInput } from './types'

export function ErrorAnalysis() {
  const { t } = useTranslation()
  const client = useQueryClient()
  const [tab, setTab] = useState('statistics')
  const [configure, setConfigure] = useState(false)
  const [selected, setSelected] = useState('')
  const [range, setRange] = useState('1')
  const [requestId, setRequestId] = useState('')
  const [model, setModel] = useState('')
  const [channel, setChannel] = useState('')
  const [offset, setOffset] = useState(0)
  const [category, setCategory] = useState('')
  const [filter, setFilter] = useState<AnalysisFilter>(() => {
    const end = Math.floor(Date.now() / 1000)
    return { start: end - 86400, end }
  })
  const config = useQuery({
    queryKey: ['error-analysis', 'config'],
    queryFn: getConfig,
  })
  const dashboard = useQuery({
    queryKey: ['error-analysis', 'statistics', filter, offset, category],
    queryFn: ({ signal }) => getAnalysis(filter, offset, category, signal),
    enabled: tab === 'statistics',
    staleTime: 15000,
  })
  const save = useMutation({
    mutationFn: (input: ConfigInput) => saveConfig(input),
    onSuccess: (data) => {
      client.setQueryData(['error-analysis', 'config'], data)
      setConfigure(false)
      toast.success(t('Analysis settings saved'))
    },
  })
  const start = useMutation({
    mutationFn: startAnalysis,
    onSuccess: (data) => {
      setSelected(data.task.task_id)
      setTab('reports')
      client.setQueryData(
        ['error-analysis', 'report', data.task.task_id],
        data.task
      )
      void client.invalidateQueries({ queryKey: ['error-analysis', 'reports'] })
      if (!data.created) {
        toast.info(t('An analysis is already active. Showing its progress.'))
      }
    },
  })
  const applyFilter = () => {
    const end = Math.floor(Date.now() / 1000)
    setOffset(0)
    setCategory('')
    setTab('statistics')
    setFilter({
      request_id: requestId.trim(),
      start: end - Number(range) * 86400,
      end,
      model: model.trim(),
      channel: Number(channel) || 0,
    })
  }
  return (
    <SectionPageLayout stackActionsOnMobile>
      <SectionPageLayout.Title>{t('Error analysis')}</SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        <Button
          size='sm'
          variant='outline'
          disabled={config.isPending || !config.data}
          onClick={() => {
            save.reset()
            setConfigure(true)
          }}
        >
          <Settings2 aria-hidden />
          {t('Analysis settings')}
        </Button>
        <Button
          size='sm'
          disabled={start.isPending || !config.data?.api_key_configured}
          onClick={() => start.mutate(filter)}
        >
          <Play aria-hidden />
          {start.isPending ? t('Starting...') : t('Start analysis')}
        </Button>
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <div className='min-w-0 space-y-5'>
          <p className='text-muted-foreground text-sm'>
            {t(
              'Find zero-output requests, interrupted streams and request errors, then investigate with a persistent AI report.'
            )}
          </p>
          {config.isError && (
            <ErrorState
              description={t('Unable to load analysis settings')}
              onRetry={() => void config.refetch()}
            />
          )}
          {config.data && (
            <div className='flex flex-wrap items-center justify-between gap-2 text-sm'>
              <span>
                {config.data.enabled
                  ? t('Daily analysis at {{hour}}:00 ({{timezone}})', {
                      hour: String(config.data.hour).padStart(2, '0'),
                      timezone: config.data.timezone,
                    })
                  : t('Automatic daily analysis is disabled')}
              </span>
              <span className='text-muted-foreground break-all'>
                {config.data.api_key_configured
                  ? config.data.model
                  : t('Configure an API key to enable AI analysis')}
              </span>
            </div>
          )}
          <form
            className='flex flex-wrap items-end gap-3 rounded-lg border p-3'
            onSubmit={(event) => {
              event.preventDefault()
              applyFilter()
            }}
          >
            <div className='w-full min-w-0 space-y-1.5 sm:w-80'>
              <Label htmlFor='analysis-request-id'>{t('Request ID')}</Label>
              <Input
                id='analysis-request-id'
                value={requestId}
                maxLength={64}
                onChange={(event) => setRequestId(event.target.value)}
                placeholder={t('Exact request ID')}
              />
            </div>
            <div className='w-full space-y-1.5 sm:w-auto'>
              <Label htmlFor='analysis-range'>{t('Time range')}</Label>
              <NativeSelect
                disabled={!!requestId.trim()}
                id='analysis-range'
                value={range}
                onChange={(event) => setRange(event.target.value)}
              >
                <NativeSelectOption value='1'>
                  {t('Last 24 hours')}
                </NativeSelectOption>
                <NativeSelectOption value='7'>
                  {t('Last 7 days')}
                </NativeSelectOption>
                <NativeSelectOption value='30'>
                  {t('Last 30 days')}
                </NativeSelectOption>
              </NativeSelect>
            </div>
            <div className='w-full min-w-0 space-y-1.5 sm:w-auto sm:min-w-40 sm:flex-1'>
              <Label htmlFor='analysis-model'>{t('Model')}</Label>
              <Input
                disabled={!!requestId.trim()}
                id='analysis-model'
                value={model}
                maxLength={200}
                onChange={(event) => setModel(event.target.value)}
                placeholder={t('All models')}
              />
            </div>
            <div className='w-28 space-y-1.5'>
              <Label htmlFor='analysis-channel'>{t('Channel ID')}</Label>
              <Input
                disabled={!!requestId.trim()}
                id='analysis-channel'
                type='number'
                min={0}
                step={1}
                value={channel}
                onChange={(event) => setChannel(event.target.value)}
                placeholder={t('All')}
              />
            </div>
            <Button
              type='submit'
              size='sm'
              variant='outline'
              disabled={dashboard.isFetching}
            >
              <RefreshCw aria-hidden />
              {t('Apply filters')}
            </Button>
          </form>
          <p className='text-muted-foreground text-xs'>
            {requestId.trim()
              ? t(
                  'Request ID searches all retained logs and overrides time, model and channel filters.'
                )
              : t('Filters apply to statistics and new analyses.')}
          </p>
          <Tabs value={tab} onValueChange={(value) => setTab(String(value))}>
            <TabsList>
              <TabsTrigger value='statistics'>
                {t('Failure statistics')}
              </TabsTrigger>
              <TabsTrigger value='reports'>
                {t('AI analysis & history')}
              </TabsTrigger>
            </TabsList>
            <TabsContent value='statistics' className='mt-5'>
              {dashboard.isPending && <LoadingState />}
              {dashboard.isError && (
                <ErrorState onRetry={() => void dashboard.refetch()} />
              )}
              {dashboard.data && !dashboard.isError && (
                <FailureDashboard
                  key={JSON.stringify(filter)}
                  data={dashboard.data.snapshot}
                  total={dashboard.data.log_total}
                  offset={offset}
                  category={category}
                  onPage={setOffset}
                  onCategory={(value) => {
                    setCategory(value)
                    setOffset(0)
                  }}
                />
              )}
            </TabsContent>
            <TabsContent value='reports' className='mt-5'>
              <AnalysisReports
                selected={selected}
                onSelect={setSelected}
                onRerun={(range) => start.mutate(range)}
                pending={start.isPending}
                configured={!!config.data?.api_key_configured}
              />
            </TabsContent>
          </Tabs>
          {configure && config.data && (
            <ConfigDialog
              config={config.data}
              pending={save.isPending}
              error={save.isError}
              onClose={() => setConfigure(false)}
              onSave={(input) => save.mutate(input)}
            />
          )}
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
