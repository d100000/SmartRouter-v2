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
import {
  QueryClient,
  QueryClientProvider,
  useQuery,
} from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { DEFAULT_LOGO } from '@/lib/constants'
import { statusQueryOptions } from '@/lib/status-query'
import { useSystemConfigStore } from '@/stores/system-config-store'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { SystemInfoSection } from '../system-info-section'

const existingLogo = 'https://example.com/existing-logo.svg'
const png = new File(['image bytes'], 'logo.png', { type: 'image/png' })
let serverLogo = existingLogo
let imageMode: 'load' | 'error' | 'pending' = 'load'
let imageWidth = 64
let completeDecode: (() => void) | undefined
const clients: QueryClient[] = []

// jsdom cannot decode images; keep file reading, forms, mutations and status real.
class DecodedImage extends EventTarget {
  naturalWidth = imageWidth
  naturalHeight = 64
  set src(_value: string) {
    completeDecode = () => {
      this.dispatchEvent(new Event('load'))
    }
    if (imageMode === 'pending') return
    queueMicrotask(() => {
      this.dispatchEvent(new Event(imageMode === 'error' ? 'error' : 'load'))
    })
  }
}

function Fixture() {
  const [container, setContainer] = useState<HTMLDivElement | null>(null)
  useQuery(statusQueryOptions)
  const appliedLogo = useSystemConfigStore((state) => state.config.logo)
  return (
    <>
      <img src={appliedLogo} alt='Applied logo' />
      <div ref={setContainer} />
      <SettingsPageProvider actionsContainer={container}>
        <SystemInfoSection
          defaultValues={{
            SystemName: 'New API',
            Logo: existingLogo,
            TaskPublicAddress: '',
            general_setting: { docs_link: '' },
            legal: { user_agreement: '', privacy_policy: '' },
          }}
        />
      </SettingsPageProvider>
    </>
  )
}

async function renderSettings() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  const router = createRouter({
    routeTree: createRootRoute({ component: Fixture }),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
  return screen.findByLabelText<HTMLInputElement>('Choose logo image')
}

beforeEach(() => {
  serverLogo = existingLogo
  imageMode = 'load'
  imageWidth = 64
  completeDecode = undefined
  vi.stubGlobal('Image', DecodedImage)
  vi.spyOn(api, 'get').mockImplementation(async () => ({
    data: { success: true, data: { logo: serverLogo } },
  }))
  vi.spyOn(api, 'put').mockImplementation(async (_url, body) => {
    serverLogo = (body as { value: string }).value
    return { data: { success: true } }
  })
})

afterEach(() => {
  clients.splice(0).forEach((client) => client.clear())
  vi.unstubAllGlobals()
})

test('choosing an image previews it locally, then Save persists it and refreshes the applied logo', async () => {
  const user = userEvent.setup()
  const input = await renderSettings()
  expect(
    screen.queryByRole('textbox', { name: 'Logo URL' })
  ).not.toBeInTheDocument()
  expect(screen.getByRole('img', { name: 'Logo preview' })).toHaveAttribute(
    'src',
    existingLogo
  )
  await user.upload(input, png)
  const preview = screen.getByRole('img', { name: 'Logo preview' })
  await waitFor(() =>
    expect(preview.getAttribute('src')).toMatch(/^data:image\/png;base64,/)
  )
  expect(api.put).not.toHaveBeenCalled()
  expect(screen.getByRole('img', { name: 'Applied logo' })).toHaveAttribute(
    'src',
    existingLogo
  )
  await user.click(screen.getByRole('button', { name: 'Save Changes' }))
  await waitFor(() =>
    expect(api.put).toHaveBeenCalledWith('/api/option/', {
      key: 'Logo',
      value: preview.getAttribute('src'),
    })
  )
  await waitFor(() =>
    expect(screen.getByRole('img', { name: 'Applied logo' })).toHaveAttribute(
      'src',
      preview.getAttribute('src')
    )
  )
  expect(screen.getByRole('button', { name: 'Reset' })).toBeDisabled()
})

test('Reset discards an upload and restoring the default only applies after Save', async () => {
  const user = userEvent.setup()
  const input = await renderSettings()
  await user.upload(input, png)
  await waitFor(() =>
    expect(
      screen.getByRole('img', { name: 'Logo preview' }).getAttribute('src')
    ).toMatch(/^data:/)
  )
  await user.click(screen.getByRole('button', { name: 'Reset' }))
  expect(screen.getByRole('img', { name: 'Logo preview' })).toHaveAttribute(
    'src',
    existingLogo
  )
  expect(api.put).not.toHaveBeenCalled()
  await user.click(screen.getByRole('button', { name: 'Use default logo' }))
  expect(screen.getByRole('img', { name: 'Logo preview' })).toHaveAttribute(
    'src',
    DEFAULT_LOGO
  )
  expect(
    screen.getByRole('button', { name: 'Use default logo' })
  ).toBeDisabled()
  expect(api.put).not.toHaveBeenCalled()
  await user.click(screen.getByRole('button', { name: 'Save Changes' }))
  await waitFor(() =>
    expect(api.put).toHaveBeenCalledWith('/api/option/', {
      key: 'Logo',
      value: '',
    })
  )
  await waitFor(() =>
    expect(screen.getByRole('img', { name: 'Applied logo' })).toHaveAttribute(
      'src',
      DEFAULT_LOGO
    )
  )
})

test.each([
  {
    name: 'unsupported type',
    file: new File(['svg'], 'logo.svg', { type: 'image/svg+xml' }),
    error: 'Choose a PNG, JPG, or WebP image',
  },
  {
    name: 'oversized file',
    file: new File([new Uint8Array(512 * 1024 + 1)], 'large.png', {
      type: 'image/png',
    }),
    error: 'Logo image must be 512 KB or smaller',
  },
  {
    name: 'corrupt image',
    file: png,
    mode: 'error' as const,
    error: 'Unable to read this image. Choose a valid PNG, JPG, or WebP file.',
  },
  {
    name: 'excessive dimensions',
    file: png,
    width: 2049,
    error: 'Logo dimensions must not exceed 2048 × 2048 pixels',
  },
])(
  '$name shows an accessible error and preserves the current logo',
  async (tc) => {
    imageMode = tc.mode ?? 'load'
    imageWidth = tc.width ?? 64
    const user = userEvent.setup({ applyAccept: false })
    const input = await renderSettings()
    await user.upload(input, tc.file)
    expect(await screen.findByText(tc.error)).toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: 'Replace image' })
    ).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByRole('img', { name: 'Logo preview' })).toHaveAttribute(
      'src',
      existingLogo
    )
    expect(api.put).not.toHaveBeenCalled()
  }
)

test('reading an image disables replacement, Reset and Save and blocks form submission until decoding completes', async () => {
  imageMode = 'pending'
  const user = userEvent.setup()
  const input = await renderSettings()
  await user.upload(input, png)
  await waitFor(() => expect(completeDecode).toBeDefined())
  expect(screen.getByRole('status')).toHaveTextContent('Reading image…')
  for (const name of ['Replace image', 'Reset', 'Save Changes']) {
    expect(screen.getByRole('button', { name })).toBeDisabled()
  }
  const form = screen
    .getByRole('textbox', { name: 'System Name' })
    .closest('form')
  if (!form) throw new Error('Settings form is missing')
  fireEvent.submit(form)
  expect(api.put).not.toHaveBeenCalled()
  await act(async () => {
    completeDecode?.()
  })
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Save Changes' })).toBeEnabled()
  )
  await user.click(screen.getByRole('button', { name: 'Save Changes' }))
  await waitFor(() =>
    expect(api.put).toHaveBeenCalledWith(
      '/api/option/',
      expect.objectContaining({ key: 'Logo' })
    )
  )
})

test('a failed save retains the uploaded draft and applied logo so the user can retry', async () => {
  vi.mocked(api.put).mockRejectedValueOnce(new Error('Storage unavailable'))
  const user = userEvent.setup()
  const input = await renderSettings()
  await user.upload(input, png)
  const preview = screen.getByRole('img', { name: 'Logo preview' })
  await waitFor(() => expect(preview.getAttribute('src')).toMatch(/^data:/))
  await user.click(screen.getByRole('button', { name: 'Save Changes' }))
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Save Changes' })).toBeEnabled()
  )
  expect(screen.getByRole('button', { name: 'Reset' })).toBeEnabled()
  expect(preview.getAttribute('src')).toMatch(/^data:/)
  expect(screen.getByRole('img', { name: 'Applied logo' })).toHaveAttribute(
    'src',
    existingLogo
  )
  await user.click(screen.getByRole('button', { name: 'Save Changes' }))
  await waitFor(() =>
    expect(screen.getByRole('img', { name: 'Applied logo' })).toHaveAttribute(
      'src',
      preview.getAttribute('src')
    )
  )
})
