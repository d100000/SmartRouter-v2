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
import { zodResolver } from '@hookform/resolvers/zod'
import { useMemo } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'

import {
  SettingsForm,
  SettingsControlGroup,
  SettingsSwitchContent,
  SettingsSwitchItem,
} from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useResetForm } from '../hooks/use-reset-form'
import type { RequestPolicySettings } from './defaults'
import { useSavePolicy } from './use-save-policy'

export function ChannelFundingSection({
  defaultValues,
}: {
  defaultValues: RequestPolicySettings
}) {
  const { t } = useTranslation()
  const mutation = useSavePolicy()
  const schema = z.object({
    enabled: z.boolean(),
    probe_minutes: z
      .number({ error: t('Enter a positive integer') })
      .int()
      .min(1, t('Funding check interval must be between 1 and 60 minutes'))
      .max(60, t('Funding check interval must be between 1 and 60 minutes')),
    reminder_hours: z
      .number({ error: t('Enter a positive integer') })
      .int()
      .min(1, t('Funding reminder interval must be between 1 and 720 hours'))
      .max(720, t('Funding reminder interval must be between 1 and 720 hours')),
    recovery_notify: z.boolean(),
  })
  type Values = z.infer<typeof schema>
  const defaults = useMemo(
    () => ({
      enabled: defaultValues['channel_funding_setting.enabled'],
      probe_minutes: defaultValues['channel_funding_setting.probe_minutes'],
      reminder_hours: defaultValues['channel_funding_setting.reminder_hours'],
      recovery_notify: defaultValues['channel_funding_setting.recovery_notify'],
    }),
    [defaultValues]
  )
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: defaults,
  })
  useResetForm(form, defaults)
  const save = form.handleSubmit(async (values) => {
    try {
      const changed = Object.entries(values).filter(
        ([key, value]) =>
          value !== form.formState.defaultValues?.[key as keyof Values]
      )
      if (changed.length === 0) return
      await mutation.mutateAsync(
        Object.fromEntries(
          changed.map(([key, value]) => [
            `channel_funding_setting.${key}`,
            String(value),
          ])
        )
      )
      form.reset(values)
    } catch {
      /* useSavePolicy displays the server error. */
    }
  })
  return (
    <SettingsSection title={t('Channel funding monitoring')}>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Detects explicit upstream balance and spending restrictions. No error does not mean sufficient balance.'
        )}
      </p>
      <Form {...form}>
        <SettingsForm onSubmit={save}>
          <SettingsPageFormActions
            onSave={save}
            isSaving={mutation.isPending}
            saveLabel='Save Changes'
          />
          <SettingsControlGroup>
            <FormField
              control={form.control}
              name='enabled'
              render={({ field }) => (
                <SettingsSwitchItem>
                  <SettingsSwitchContent>
                    <FormLabel>{t('Enable funding monitoring')}</FormLabel>
                    <FormDescription>
                      {t(
                        'Turning this off stops new detection, recovery checks and notifications. Existing events and channel states are retained.'
                      )}
                    </FormDescription>
                  </SettingsSwitchContent>
                  <FormControl>
                    <Switch
                      checked={field.value}
                      onCheckedChange={field.onChange}
                    />
                  </FormControl>
                </SettingsSwitchItem>
              )}
            />
            <FormField
              control={form.control}
              name='recovery_notify'
              render={({ field }) => (
                <SettingsSwitchItem>
                  <SettingsSwitchContent>
                    <FormLabel>{t('Send recovery notifications')}</FormLabel>
                    <FormDescription>
                      {t(
                        'Notify subscribed administrators who received the funding alert, using their existing notification preferences.'
                      )}
                    </FormDescription>
                  </SettingsSwitchContent>
                  <FormControl>
                    <Switch
                      checked={field.value}
                      onCheckedChange={field.onChange}
                    />
                  </FormControl>
                </SettingsSwitchItem>
              )}
            />
          </SettingsControlGroup>
          <div className='grid gap-4 sm:grid-cols-2'>
            <FormField
              control={form.control}
              name='probe_minutes'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('First recovery check (minutes)')}</FormLabel>
                  <FormControl>
                    <Input
                      type='number'
                      min={1}
                      max={60}
                      {...field}
                      onChange={(event) =>
                        field.onChange(event.target.valueAsNumber)
                      }
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name='reminder_hours'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Repeat reminder (hours)')}</FormLabel>
                  <FormControl>
                    <Input
                      type='number'
                      min={1}
                      max={720}
                      {...field}
                      onChange={(event) =>
                        field.onChange(event.target.valueAsNumber)
                      }
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
          </div>
          <div className='text-muted-foreground space-y-2 text-sm'>
            <p>
              {defaultValues.AutomaticDisableChannelEnabled
                ? t(
                    'Automatic disable is enabled globally; each channel must also allow auto-disable.'
                  )
                : t(
                    'Automatic disable is off globally. Funding events will only be marked and notified.'
                  )}
            </p>
            <p>
              {defaultValues.AutomaticEnableChannelEnabled
                ? t(
                    'Automatic enable is on. Only disables still owned by the recovered event can be reversed.'
                  )
                : t(
                    'Automatic enable is off. Recovery clears the marker and leaves the channel disabled.'
                  )}
            </p>
            <p>
              {t(
                'Checks run one at a time and back off to hourly. Text checks may incur a small charge. Image and video generation tasks are never created automatically.'
              )}
            </p>
          </div>
        </SettingsForm>
      </Form>
    </SettingsSection>
  )
}
