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
import { queryOptions } from '@tanstack/react-query'
import { z } from 'zod'

// The custom project's releases are independent of the upstream updater.
export const PROJECT_REPOSITORY_URL =
  'https://github.com/d100000/SmartRouter-v2'
const PROJECT_API_URL = 'https://api.github.com/repos/d100000/SmartRouter-v2'
const REFRESH_INTERVAL = 60 * 60 * 1000

const releaseSchema = z.object({
  tag_name: z.string().trim().min(1),
  draft: z.boolean(),
  published_at: z.iso.datetime({ offset: true }).nullable(),
})

const commitSchema = z.object({
  sha: z.string().regex(/^[a-f0-9]{40}$/i),
  commit: z.object({
    committer: z.object({ date: z.iso.datetime({ offset: true }) }),
  }),
})

type ProjectVersionInfo = {
  kind: 'release' | 'commit'
  version: string
  date: string
  url: string
}

export async function fetchProjectVersion(
  signal: AbortSignal
): Promise<ProjectVersionInfo | null> {
  const controller = new AbortController()
  const cancel = () => controller.abort()
  signal.addEventListener('abort', cancel, { once: true })
  if (signal.aborted) controller.abort()
  const timeout = setTimeout(cancel, 10_000)
  const requestOptions: RequestInit = {
    credentials: 'omit',
    headers: { Accept: 'application/vnd.github+json' },
    signal: controller.signal,
  }

  try {
    const response = await fetch(
      `${PROJECT_API_URL}/releases?per_page=100`,
      requestOptions
    )
    if (!response.ok) throw new Error('Failed to check for updates')
    const releases = z.array(releaseSchema).parse(await response.json())
    let latest: z.infer<typeof releaseSchema> | undefined
    for (const release of releases) {
      if (release.draft || !release.published_at) continue
      if (
        !latest?.published_at ||
        Date.parse(release.published_at) > Date.parse(latest.published_at)
      ) {
        latest = release
      }
    }
    if (latest?.published_at) {
      return {
        kind: 'release',
        version: latest.tag_name,
        date: latest.published_at,
        url: `${PROJECT_REPOSITORY_URL}/releases/tag/${encodeURIComponent(latest.tag_name)}`,
      }
    }

    // Until the custom project publishes releases, identify the default branch
    // by its commit and commit date rather than presenting an upstream tag.
    const commitsResponse = await fetch(
      `${PROJECT_API_URL}/commits?per_page=1`,
      requestOptions
    )
    if (commitsResponse.status === 409) return null // Empty Git repository.
    if (!commitsResponse.ok) throw new Error('Failed to check for updates')
    const [commit] = z.array(commitSchema).parse(await commitsResponse.json())
    if (!commit) return null
    return {
      kind: 'commit',
      version: commit.sha.slice(0, 7),
      date: commit.commit.committer.date,
      url: `${PROJECT_REPOSITORY_URL}/commit/${commit.sha}`,
    }
  } finally {
    clearTimeout(timeout)
    signal.removeEventListener('abort', cancel)
  }
}

export const projectVersionQueryOptions = queryOptions({
  queryKey: ['project-version', PROJECT_REPOSITORY_URL],
  queryFn: ({ signal }) => fetchProjectVersion(signal),
  staleTime: REFRESH_INTERVAL,
  gcTime: 24 * REFRESH_INTERVAL,
  refetchInterval: REFRESH_INTERVAL,
  refetchIntervalInBackground: false,
  refetchOnMount: false,
  refetchOnWindowFocus: false,
  retry: false,
  retryOnMount: false,
  meta: { errorToast: false },
})
