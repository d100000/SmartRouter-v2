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
import { render, screen } from '@testing-library/react'
import { expect, test } from 'vitest'

import { HeaderOverrideVariables } from '../header-override-variables'

test('the header override hint names every variable the gateway resolves and what a missing value does', () => {
  const { container } = render(
    <p>
      <HeaderOverrideVariables />
    </p>
  )

  // The same names as processHeaderOverride in relay/channel/api_request.go.
  expect(
    screen.getAllByText(/^\{[A-Za-z_:]+\}$/).map((code) => code.textContent)
  ).toEqual([
    '{api_key}',
    '{client_header:NAME}',
    '{user_id}',
    '{model}',
    '{original_model}',
    '{channel_id}',
    '{timestamp}',
    '{timestamp_ms}',
  ])
  expect(container).toHaveTextContent(
    'Supported variables: {api_key} — Channel key, {client_header:NAME} — Client header value, {user_id} — User ID, {model} — Upstream Model Name, {original_model} — Request Model Name, {channel_id} — Channel ID, {timestamp} — Unix timestamp (seconds), {timestamp_ms} — Unix timestamp (milliseconds)'
  )
  expect(container).toHaveTextContent(
    'A header is not sent when one of its variables has no value for the request.'
  )
})
