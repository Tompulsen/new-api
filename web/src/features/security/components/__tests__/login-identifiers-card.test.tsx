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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { LoginIdentifiersCard } from '../login-identifiers-card'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

it('shows canonical identity and only the login names returned by the account service', async () => {
  const get = vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        username: '12334',
        email: '2265377601@qq.com',
        login_identifiers: ['12334', '2265377601@qq.com', 'tomplusen'],
      },
    },
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <LoginIdentifiersCard userId={91} />
    </QueryClientProvider>
  )
  expect(await screen.findByText('tomplusen')).toBeInTheDocument()
  expect(screen.getByText('Unified account')).toBeInTheDocument()
  expect(get).toHaveBeenCalledWith('/api/user/self/login-identifiers', {
    signal: expect.any(AbortSignal),
  })
  client.clear()
})

it('does not display a previous account response after the identity changes', async () => {
  let finishOld!: (value: unknown) => void
  vi.spyOn(api, 'get')
    .mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finishOld = resolve
        })
    )
    .mockResolvedValueOnce({
      data: {
        success: true,
        data: {
          username: 'new-user',
          email: '',
          login_identifiers: ['new-alias'],
        },
      },
    })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const tree = (userId: number) => (
    <QueryClientProvider client={client}>
      <LoginIdentifiersCard userId={userId} />
    </QueryClientProvider>
  )
  const view = render(tree(7))
  view.rerender(tree(8))
  await screen.findByText('new-alias')
  await act(async () =>
    finishOld({
      data: {
        success: true,
        data: {
          username: 'old-user',
          email: '',
          login_identifiers: ['old-private-alias'],
        },
      },
    })
  )
  expect(screen.queryByText('old-private-alias')).not.toBeInTheDocument()
  expect(screen.getByText('new-alias')).toBeInTheDocument()
  client.clear()
})

it('fails closed on malformed metadata and offers retry', async () => {
  vi.spyOn(api, 'get')
    .mockResolvedValueOnce({
      data: {
        success: true,
        data: { username: 'user', email: '', login_identifiers: [7] },
      },
    })
    .mockResolvedValueOnce({
      data: {
        success: true,
        data: {
          username: 'user',
          email: '',
          login_identifiers: ['safe-alias'],
        },
      },
    })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <LoginIdentifiersCard userId={7} />
    </QueryClientProvider>
  )
  await screen.findByRole('alert')
  await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))
  expect(await screen.findByText('safe-alias')).toBeInTheDocument()
  client.clear()
})
