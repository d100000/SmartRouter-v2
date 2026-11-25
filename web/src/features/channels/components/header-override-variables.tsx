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
import { Fragment } from 'react'
import { useTranslation } from 'react-i18next'

// The variables the gateway resolves in a header override value, each with
// the translation key of what it stands for.
const VARIABLES = [
  ['{api_key}', 'Channel key'],
  ['{client_header:NAME}', 'Client header value'],
  ['{user_id}', 'User ID'],
  ['{model}', 'Upstream Model Name'],
  ['{original_model}', 'Request Model Name'],
  ['{channel_id}', 'Channel ID'],
  ['{timestamp}', 'Unix timestamp (seconds)'],
  ['{timestamp_ms}', 'Unix timestamp (milliseconds)'],
] as const

/** The hint under the header override editor: what a value may contain. */
export function HeaderOverrideVariables() {
  const { t } = useTranslation()
  return (
    <>
      {t('Supported variables')}:{' '}
      {VARIABLES.map(([variable, meaning], index) => (
        <Fragment key={variable}>
          {index > 0 && ', '}
          <code className='bg-muted rounded px-1 py-0.5'>
            {variable}
          </code> — {t(meaning)}
        </Fragment>
      ))}
      <br />
      {t(
        'A header is not sent when one of its variables has no value for the request.'
      )}
    </>
  )
}
