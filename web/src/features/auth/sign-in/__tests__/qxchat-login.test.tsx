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
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from '@tanstack/react-router'
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { STATUS_QUERY_KEY } from '@/lib/status-query'
import { useAuthStore } from '@/stores/auth-store'

import { UserAuthForm } from '../components/user-auth-form'

let client: QueryClient
beforeEach(() => {
  localStorage.clear()
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  client.setQueryData(STATUS_QUERY_KEY, {
    password_login_enabled: true,
    password_login_encryption_enabled: false,
  })
})
afterEach(() => {
  cleanup()
  client.clear()
  vi.restoreAllMocks()
  useAuthStore.getState().auth.reset('idle')
  localStorage.clear()
  delete window.turnstile
  document.querySelector('#cf-turnstile')?.remove()
})

async function renderLogin(enabled: boolean) {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: { enabled } },
  })
  const root = createRootRoute({ component: Outlet })
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({
        getParentRoute: () => root,
        path: '/sign-in',
        component: UserAuthForm,
      }),
      createRoute({
        getParentRoute: () => root,
        path: '/otp',
        component: () => <div>Complete account verification</div>,
      }),
      createRoute({
        getParentRoute: () => root,
        path: '/forgot-password',
        component: () => <div>Reset password</div>,
      }),
    ]),
    history: createMemoryHistory({ initialEntries: ['/sign-in'] }),
  })
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
  await screen.findByLabelText('Username or Email')
  return userEvent.setup()
}

it('uses the QXChat login endpoint only after selecting the available old-account option and preserves MFA', async () => {
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      data: {
        require_verification: true,
        flow_token: 'qxchat-mfa-challenge',
        expires_at: Math.floor(Date.now() / 1000) + 300,
        methods: [{ method: '2fa', available: true }],
      },
    },
  })
  const user = await renderLogin(true)
  const checkbox = await screen.findByRole('checkbox', {
    name: 'Sign in with a QXChat account',
  })
  expect(checkbox).not.toBeChecked()
  checkbox.focus()
  await user.keyboard(' ')
  expect(checkbox).toBeChecked()
  await user.type(screen.getByLabelText('Username or Email'), 'legacy-user')
  await user.type(
    screen.getByLabelText('Password', { exact: true }),
    'old-password'
  )
  await user.click(screen.getByRole('button', { name: 'Sign in' }))
  expect(await screen.findByText('Complete account verification')).toBeVisible()
  expect(post).toHaveBeenCalledWith(
    '/api/user/login/qxchat?turnstile=',
    { username: 'legacy-user', password: 'old-password' },
    { skipAuthRefresh: true }
  )
  expect(
    useAuthStore.getState().auth.pendingLoginVerification?.challenge.flow_token
  ).toBe('qxchat-mfa-challenge')
  expect(useAuthStore.getState().auth.user).toBeNull()
})

it('hides the option when disabled and retains the normal login request', async () => {
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: false, message: 'Login failed' } })
  const user = await renderLogin(false)
  await waitFor(() =>
    expect(client.getQueryData(['qxchat', 'config'])).toEqual({
      enabled: false,
    })
  )
  expect(
    screen.queryByRole('checkbox', { name: 'Sign in with a QXChat account' })
  ).not.toBeInTheDocument()
  await user.type(screen.getByLabelText('Username or Email'), 'newapi-user')
  await user.type(
    screen.getByLabelText('Password', { exact: true }),
    'new-password'
  )
  await user.click(screen.getByRole('button', { name: 'Sign in' }))
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith(
      '/api/user/login?turnstile=',
      { username: 'newapi-user', password: 'new-password' },
      { skipAuthRefresh: true }
    )
  )
})

it.each([false, true])(
  'requires a fresh verification after a script failure for QXChat login = %s',
  async (qxchat) => {
    client.setQueryData(STATUS_QUERY_KEY, {
      password_login_enabled: true,
      turnstile_check: true,
      turnstile_site_key: 'test-site',
    })
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: false, message: 'Login failed' } })
    const user = await renderLogin(true)
    if (qxchat) {
      await user.click(
        await screen.findByRole('checkbox', {
          name: 'Sign in with a QXChat account',
        })
      )
    }
    await user.type(screen.getByLabelText('Username or Email'), 'existing-user')
    await user.type(
      screen.getByLabelText('Password', { exact: true }),
      'existing-password'
    )
    const script = document.querySelector('#cf-turnstile')
    if (!script) throw new Error('Turnstile script was not created')
    fireEvent.error(script)
    await user.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(post).not.toHaveBeenCalled()
    await user.click(screen.getByRole('button', { name: 'Retry' }))
    expect(post).not.toHaveBeenCalled()
    let options: Record<string, unknown> = {}
    window.turnstile = {
      render: (_element, value) => {
        options = value
        return 'recovered-widget'
      },
      remove: vi.fn(),
    }
    const retryScript = document.querySelector('#cf-turnstile')
    if (!retryScript) throw new Error('Turnstile script was not retried')
    fireEvent.load(retryScript)
    await user.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(post).not.toHaveBeenCalled()
    act(() => (options.callback as (token: string) => void)('fresh-token'))
    await user.click(screen.getByRole('button', { name: 'Sign in' }))
    await waitFor(() =>
      expect(post).toHaveBeenCalledWith(
        `${qxchat ? '/api/user/login/qxchat' : '/api/user/login'}?turnstile=fresh-token`,
        { username: 'existing-user', password: 'existing-password' },
        { skipAuthRefresh: true }
      )
    )
  }
)
