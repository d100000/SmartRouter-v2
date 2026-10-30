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
import { QueryClientProvider, type QueryClient } from '@tanstack/react-query'
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { createAppQueryClient } from '@/lib/query-client'

import { ProjectVersion } from '../project-version'
import {
  fetchProjectVersion,
  PROJECT_REPOSITORY_URL,
  projectVersionQueryOptions,
} from '../query'

const fetchMock = vi.fn<typeof fetch>()
const commit = {
  sha: '112f0a39abd339e6e8bbd97f770c497248e85c54',
  commit: { committer: { date: '2026-10-28T08:00:00Z' } },
}
const release = {
  tag_name: 'custom/v2026.10.29',
  draft: false,
  prerelease: true,
  published_at: '2026-10-29T08:00:00Z',
}
let client: QueryClient

function Wrapper(props: { children: ReactNode }) {
  return (
    <QueryClientProvider client={client}>{props.children}</QueryClientProvider>
  )
}

beforeEach(() => {
  client = createAppQueryClient()
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
  vi.stubEnv('VITE_PROJECT_VERSION', 'df5fe14-local')
  vi.stubEnv('VITE_PROJECT_VERSION_DATE', '2026-10-28T08:00:00Z')
})

afterEach(() => {
  cleanup()
  client.clear()
  vi.unstubAllGlobals()
  vi.unstubAllEnvs()
  vi.useRealTimers()
})

describe('custom project version footer', () => {
  test('shows the installed build separately from the latest published release and links to the custom repository', async () => {
    fetchMock.mockResolvedValue(
      new Response(
        JSON.stringify([
          {
            ...release,
            tag_name: 'older',
            published_at: '2026-10-20T08:00:00Z',
          },
          release,
          {
            ...release,
            tag_name: 'draft',
            draft: true,
            published_at: '2026-10-30T08:00:00Z',
          },
        ])
      )
    )
    render(<ProjectVersion />, { wrapper: Wrapper })

    expect(screen.getByText('df5fe14-local')).toBeInTheDocument()
    expect(screen.getByText('2026-10-28')).toHaveAttribute(
      'datetime',
      '2026-10-28T08:00:00Z'
    )
    const link = await screen.findByRole('link', {
      name: /Latest release custom\/v2026.10.29/,
    })
    expect(link).toHaveAttribute(
      'href',
      `${PROJECT_REPOSITORY_URL}/releases/tag/custom%2Fv2026.10.29`
    )
    expect(link).toHaveAttribute('target', '_blank')
    expect(link).toHaveAttribute('rel', 'noopener noreferrer')
    expect(screen.getByText('2026-10-29')).toHaveAttribute(
      'datetime',
      release.published_at
    )
    expect(screen.getByText('df5fe14-local')).toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock).toHaveBeenCalledWith(
      'https://api.github.com/repos/d100000/SmartRouter-v2/releases?per_page=100',
      expect.objectContaining({ credentials: 'omit' })
    )
    await userEvent.setup().tab()
    expect(link).toHaveFocus()
  })

  test('falls back to the default branch commit and its date when there are no releases', async () => {
    fetchMock.mockResolvedValueOnce(new Response('[]'))
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify([commit])))
    render(<ProjectVersion />, { wrapper: Wrapper })
    const link = await screen.findByRole('link', {
      name: /Latest commit 112f0a3/,
    })
    expect(link).toHaveAttribute(
      'href',
      `${PROJECT_REPOSITORY_URL}/commit/${commit.sha}`
    )
    expect(link.querySelector('time')).toHaveAttribute(
      'datetime',
      commit.commit.committer.date
    )
    expect(fetchMock).toHaveBeenLastCalledWith(
      'https://api.github.com/repos/d100000/SmartRouter-v2/commits?per_page=1',
      expect.objectContaining({ credentials: 'omit' })
    )
  })

  test('keeps the current build visible while loading and allows version metadata to wrap', async () => {
    let respond!: (response: Response) => void
    fetchMock.mockReturnValue(
      new Promise((resolve) => {
        respond = resolve
      })
    )
    const view = render(<ProjectVersion />, { wrapper: Wrapper })
    expect(screen.getByText('df5fe14-local')).toBeInTheDocument()
    expect(
      screen.getByRole('link', { name: 'Checking updates...' })
    ).toHaveAttribute('href', PROJECT_REPOSITORY_URL)
    expect(screen.getByRole('group', { name: 'Project version' })).toHaveClass(
      'flex-wrap',
      'min-w-0',
      'justify-end'
    )
    await act(async () => respond(new Response(JSON.stringify([release]))))
    expect(await screen.findByText(release.tag_name)).toHaveClass(
      'break-all',
      'min-w-0'
    )
    view.unmount()
  })

  test.each([403, 429, 500])(
    'keeps the installed version when GitHub responds with %s, without retrying on remount',
    async (status) => {
      fetchMock.mockResolvedValue(new Response('{}', { status }))
      const view = render(<ProjectVersion />, { wrapper: Wrapper })
      const link = await screen.findByRole('link', {
        name: 'GitHub temporarily unavailable',
      })
      expect(link).toHaveAttribute('href', PROJECT_REPOSITORY_URL)
      expect(screen.getByText('df5fe14-local')).toBeInTheDocument()
      view.unmount()
      render(<ProjectVersion />, { wrapper: Wrapper })
      expect(
        screen.getByRole('link', { name: 'GitHub temporarily unavailable' })
      ).toBeInTheDocument()
      expect(fetchMock).toHaveBeenCalledTimes(1)
    }
  )

  test('retains a previously fetched version and marks it as last known after a failed refresh', async () => {
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify([release])))
    render(<ProjectVersion />, { wrapper: Wrapper })
    await screen.findByRole('link', { name: /Latest release/ })
    fetchMock.mockRejectedValueOnce(new TypeError('offline'))
    await act(async () => {
      await client.refetchQueries({
        queryKey: projectVersionQueryOptions.queryKey,
      })
    })
    const link = await screen.findByRole('link', { name: /Last known version/ })
    expect(link).toHaveTextContent(release.tag_name)
    expect(link).toHaveAttribute(
      'href',
      `${PROJECT_REPOSITORY_URL}/releases/tag/custom%2Fv2026.10.29`
    )
  })

  test('deduplicates the homepage and console version queries', async () => {
    fetchMock.mockImplementation(
      async () => new Response(JSON.stringify([release]))
    )
    render(
      <>
        <ProjectVersion />
        <ProjectVersion />
      </>,
      { wrapper: Wrapper }
    )
    await waitFor(() =>
      expect(
        screen.getAllByRole('link', { name: /Latest release/ })
      ).toHaveLength(2)
    )
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  test('handles source builds without metadata and empty GitHub repositories', async () => {
    vi.stubEnv('VITE_PROJECT_VERSION', '')
    vi.stubEnv('VITE_PROJECT_VERSION_DATE', 'invalid')
    fetchMock.mockResolvedValueOnce(new Response('[]'))
    fetchMock.mockResolvedValueOnce(new Response('{}', { status: 409 }))
    render(<ProjectVersion />, { wrapper: Wrapper })
    expect(screen.getByText('Local build')).toBeInTheDocument()
    expect(
      await screen.findByRole('link', { name: 'No published versions' })
    ).toHaveAttribute('href', PROJECT_REPOSITORY_URL)
    expect(screen.queryByText('Invalid Date')).not.toBeInTheDocument()
  })

  test('does not invent a version or request commits when GitHub returns malformed release data', async () => {
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify([{ ...release, published_at: 'bad date' }]))
    )
    await expect(
      fetchProjectVersion(new AbortController().signal)
    ).rejects.toThrow()
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  test('aborts a stalled GitHub request after the timeout', async () => {
    vi.useFakeTimers()
    fetchMock.mockImplementation(
      (_input, options) =>
        new Promise((_resolve, reject) => {
          options?.signal?.addEventListener('abort', () =>
            reject(new DOMException('Aborted', 'AbortError'))
          )
        })
    )
    const request = fetchProjectVersion(new AbortController().signal)
    const assertion = expect(request).rejects.toThrow('Aborted')
    await vi.advanceTimersByTimeAsync(10_000)
    await assertion
    expect(fetchMock.mock.calls[0][1]?.signal?.aborted).toBe(true)
  })
})
