/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.
*/
import { act, render, screen } from '@testing-library/react'
import i18next from 'i18next'
import { afterEach, describe, expect, it } from 'vitest'

import { PerformanceCollectionHealthStatus } from '../components/performance-collection-health'
import { formatPerformanceTimestamp, formatThroughput } from '../lib/format'
import type { PerformanceCollectionHealth } from '../types'

const sampleAt = 1789387200

afterEach(async () => {
  await act(() => i18next.changeLanguage('en'))
  i18next.removeResourceBundle('zhCN', 'translation')
})

describe('performance collection timestamps', () => {
  it('changes the accessible status to an alert when projection falls behind', () => {
    const collection: PerformanceCollectionHealth = {
      enabled: true,
      availability: 'ready',
      storage: 'database_outbox',
      target_visibility_seconds: 60,
      pending_events: 0,
      oldest_pending_at: null,
      last_sample_at: sampleAt,
      last_accepted_at: sampleAt,
      last_projected_at: sampleAt,
      persisted_through: null,
      collector_heartbeat_at: sampleAt,
      unpersisted_events: 0,
      oldest_unpersisted_at: null,
      dropped_events: 0,
      stale_collectors: 0,
      local_unpersisted_events: 0,
    }
    const result = render(
      <PerformanceCollectionHealthStatus collection={collection} />
    )
    expect(
      screen.getByRole('status', { name: 'Collection health' })
    ).toHaveTextContent('Collection is active')
    expect(
      screen.getByText(
        'Newly accepted samples normally appear within 60 seconds.'
      )
    ).toBeVisible()

    result.rerender(
      <PerformanceCollectionHealthStatus
        collection={{
          ...collection,
          availability: 'delayed',
          pending_events: 4,
          oldest_pending_at: sampleAt,
        }}
      />
    )

    expect(
      screen.queryByRole('status', { name: 'Collection health' })
    ).not.toBeInTheDocument()
    expect(
      screen.getByRole('alert', { name: 'Collection health' })
    ).toHaveTextContent('Projection is delayed')
    expect(
      screen.getByRole('definition', { name: 'Pending projection' })
    ).toHaveTextContent('4')
  })

  it.each([
    ['zhCN', 'zh-CN'],
    ['zhTW', 'zh-TW'],
    ['en', 'en'],
    ['fr', 'fr'],
    ['ja', 'ja'],
    ['ru', 'ru'],
    ['vi', 'vi'],
  ])(
    'formats sample time for interface language %s using a valid Intl locale',
    (language, locale) => {
      const expected = new Intl.DateTimeFormat(locale, {
        dateStyle: 'short',
        timeStyle: 'long',
      }).format(new Date(sampleAt * 1000))
      expect(formatPerformanceTimestamp(sampleAt, language)).toBe(expected)
    }
  )

  it('falls back for invalid language tags and leaves missing or invalid times unknown', () => {
    const expected = new Intl.DateTimeFormat(undefined, {
      dateStyle: 'short',
      timeStyle: 'long',
    }).format(new Date(sampleAt * 1000))
    expect(formatPerformanceTimestamp(sampleAt, 'invalid_locale!')).toBe(
      expected
    )
    for (const value of [
      undefined,
      null,
      0,
      -1,
      Number.NaN,
      Number.POSITIVE_INFINITY,
      Number.MAX_SAFE_INTEGER,
    ]) {
      expect(formatPerformanceTimestamp(value, 'zhTW')).toBe('—')
    }
  })

  it('updates the displayed sample timestamp when the interface language changes', async () => {
    i18next.addResourceBundle('zhCN', 'translation', {
      'Collection health': '采集状态',
    })
    render(<PerformanceCollectionHealthStatus lastSampleAt={sampleAt} />)
    const english = screen.getByRole('definition', {
      name: 'Latest sample',
    }).textContent

    await act(() => i18next.changeLanguage('zhCN'))

    expect(screen.getByRole('status', { name: '采集状态' })).toBeVisible()
    expect(
      screen.getByRole('definition', { name: 'Latest sample' })
    ).toHaveTextContent(formatPerformanceTimestamp(sampleAt, 'zhCN'))
    expect(
      screen.getByRole('definition', { name: 'Latest sample' }).textContent
    ).not.toBe(english)
  })

  it('formats a known zero throughput only when explicitly enabled by reliability metrics', () => {
    expect(formatThroughput(0)).toBe('—')
    expect(formatThroughput(0, true)).toBe('0 t/s')
    expect(formatThroughput(Number.NaN, true)).toBe('—')
    expect(formatThroughput(-1, true)).toBe('—')
    expect(formatThroughput(12, true)).toBe(formatThroughput(12))
  })
})
