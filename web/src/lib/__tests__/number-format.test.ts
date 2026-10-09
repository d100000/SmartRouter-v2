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
import { describe, expect, test } from 'vitest'

import { toIntlLocale } from '@/i18n/languages'

import { formatNumber } from '../format'

describe('number precision with interface locales', () => {
  test.each([
    ['zhCN', '1.23', '0.0015'],
    ['zhTW', '1.23', '0.0015'],
    ['en', '1.23', '0.0015'],
    ['fr', '1,23', '0,0015'],
    ['ru', '1,23', '0,0015'],
    ['ja', '1.23', '0.0015'],
    ['vi', '1,23', '0,0015'],
  ])(
    '%s retains ordinary precision and displays small explicit cost ratios',
    (language, ordinary, costRatio) => {
      const locale = toIntlLocale(language)
      expect(formatNumber(1.23456, locale)).toBe(ordinary)
      expect(
        formatNumber(1.23456, locale, { maximumFractionDigits: undefined })
      ).toBe(ordinary)
      expect(formatNumber(0.0015, locale, { maximumFractionDigits: 20 })).toBe(
        costRatio
      )
    }
  )

  test('invalid interface languages use the runtime locale without throwing', () => {
    expect(toIntlLocale('not_a_valid_language')).toBeUndefined()
    expect(
      formatNumber(0.0015, toIntlLocale('not_a_valid_language'), {
        maximumFractionDigits: 20,
      })
    ).toBe(formatNumber(0.0015, undefined, { maximumFractionDigits: 20 }))
  })
})
