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

export function SupplierChannelTestStatus(props: { status: string }) {
  const { t } = useTranslation()
  return <>{validationState(props.status, t)}</>
}

function validationState(status: string, t: (key: string) => string): string {
  switch (status) {
    case 'validated':
      return t('Validated')
    case 'ready_for_review':
      return t('Test passed, awaiting publication')
    case 'closed':
      return t('Draft closed')
    case 'failed':
      return t('Validation failed')
    case 'testing':
      return t('Testing')
    case 'waiting':
      return t('Waiting for task completion')
    case 'result_unknown':
      return t('Validation result unknown')
    case 'model_price_error':
      return t('Waiting for model pricing')
    case 'cost_config_missing':
      return t('Waiting for cost configuration')
    case 'pending':
      return t('Pending validation')
    case 'configuring':
      return t('Awaiting test')
    default:
      return t('Unknown')
  }
}
