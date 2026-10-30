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
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { SSE } from 'sse.js'

import { getFreshAuthHeaders } from '@/lib/api'

import { getReport } from './api'
import type { AnalysisTask } from './types'

export function useAnalysisStream(id: string) {
  const client = useQueryClient()
  const [connection, setConnection] = useState<
    'connecting' | 'live' | 'polling'
  >('connecting')
  const query = useQuery({
    queryKey: ['error-analysis', 'report', id],
    queryFn: () => getReport(id),
    enabled: !!id,
    refetchInterval: (query) => {
      const status = query.state.data?.status
      return status === 'pending' || status === 'running' ? 5000 : false
    },
  })
  const active =
    query.data?.status === 'pending' || query.data?.status === 'running'
  useEffect(() => {
    if (!id || !active) return
    let disposed = false
    let source: SSE | undefined
    let timeout: ReturnType<typeof setTimeout> | undefined
    setConnection('connecting')
    const fallback = () => {
      source?.close()
      if (!disposed) setConnection('polling')
    }
    void getFreshAuthHeaders()
      .then((headers) => {
        if (disposed) return
        source = new SSE(
          `/api/error-analysis/reports/${encodeURIComponent(id)}/stream`,
          { headers, withCredentials: true, start: false }
        )
        timeout = setTimeout(fallback, 20000)
        source.addEventListener('snapshot', (event: MessageEvent<string>) => {
          if (disposed) return
          try {
            const task = JSON.parse(event.data) as AnalysisTask
            if (task.task_id !== id) return
            clearTimeout(timeout)
            timeout = setTimeout(fallback, 20000)
            client.setQueryData(['error-analysis', 'report', id], task)
            setConnection('live')
            if (task.status === 'succeeded' || task.status === 'failed') {
              source?.close()
              void client.invalidateQueries({
                queryKey: ['error-analysis', 'reports'],
              })
            }
          } catch {
            fallback()
          }
        })
        source.addEventListener('error', fallback)
        source.stream()
      })
      .catch(fallback)
    return () => {
      disposed = true
      clearTimeout(timeout)
      source?.close()
    }
  }, [id, active, client])
  return { ...query, connection }
}
