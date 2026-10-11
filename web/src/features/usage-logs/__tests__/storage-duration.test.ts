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

import { formatStorageDuration } from '../lib/format'

describe('explicit cache storage duration', () => {
  test.each([
    [0, '0 seconds'],
    [59, '59 seconds'],
    [60, '1 minute (60 seconds)'],
    [1800, '30 minutes (1,800 seconds)'],
    [3600, '1 hour (3,600 seconds)'],
    [5400, '1.5 hours (5,400 seconds)'],
    [86400, '1 day (86,400 seconds)'],
    [90000, '25 hours (90,000 seconds)'],
    [172800, '2 days (172,800 seconds)'],
  ])('formats %i seconds as %s', (seconds, expected) => {
    expect(formatStorageDuration(seconds, 'en')).toBe(expected)
  })

  test.each([
    ['zhCN', '小时', '秒'],
    ['zhTW', '小時', '秒'],
    ['fr', 'heure', 'secondes'],
    ['ja', '時間', '秒'],
    ['ru', 'час', 'секунд'],
    ['vi', 'giờ', 'giây'],
    ['en', 'hour', 'seconds'],
  ])(
    'localizes units for the %s interface language',
    (language, hour, second) => {
      const text = formatStorageDuration(3600, toIntlLocale(language))
      expect(text).toContain(hour)
      expect(text).toContain(second)
    }
  )

  test('falls back to the runtime locale for an invalid interface language', () => {
    expect(formatStorageDuration(3600, toIntlLocale('invalid_locale'))).toBe(
      formatStorageDuration(3600, undefined)
    )
  })
})
