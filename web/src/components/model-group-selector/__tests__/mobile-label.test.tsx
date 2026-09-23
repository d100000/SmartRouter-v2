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
import { afterEach, expect, test, vi } from 'vitest'

import { GroupSelector, ModelSelector } from '@/components/model-group-selector'

afterEach(() => vi.unstubAllGlobals())

test('existing selector consumers retain compact mobile triggers when label opt-in is omitted', () => {
  vi.stubGlobal('innerWidth', 390)
  render(
    <>
      <GroupSelector
        selectedGroup='default'
        groups={[{ value: 'default', label: 'default' }]}
        onGroupChange={() => undefined}
      />
      <ModelSelector
        selectedModel='model-a'
        models={[{ value: 'model-a', label: 'model-a' }]}
        onModelChange={() => undefined}
      />
    </>
  )
  for (const value of ['default', 'model-a']) {
    const label = screen.getByText(value)
    expect(label).toHaveClass('hidden', 'sm:block')
    expect(label.closest('button')).toHaveClass('w-8')
  }
})
