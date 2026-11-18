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

async function renderLogin(redirectTo?: string) {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: {} },
  })
  const root = createRootRoute({ component: Outlet })
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({
        getParentRoute: () => root,
        path: '/sign-in',
        component: () => <UserAuthForm redirectTo={redirectTo} />,
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
  return { user: userEvent.setup(), router }
}

const challenge = () => ({
  require_verification: true,
  flow_token: 'mfa-challenge',
  expires_at: Math.floor(Date.now() / 1000) + 300,
  methods: [{ method: '2fa', available: true }],
})

it('uses the unified endpoint without an account-source option and preserves MFA and the SSO destination', async () => {
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: challenge() } })
  const destination =
    '/community-authorize?state=preserved-state&code_challenge=preserved-pkce'
  const { user } = await renderLogin(destination)
  expect(
    screen.queryByRole('checkbox', { name: 'Sign in with a QXChat account' })
  ).not.toBeInTheDocument()
  expect(
    screen.getByRole('link', { name: 'Forgot password?' })
  ).toBeInTheDocument()
  await user.type(
    screen.getByLabelText('Username or Email'),
    '2265377601@qq.com'
  )
  await user.type(
    screen.getByLabelText('Password', { exact: true }),
    'current-password'
  )
  await user.click(screen.getByRole('button', { name: 'Sign in' }))
  await screen.findByText('Complete account verification')
  expect(post).toHaveBeenCalledWith(
    '/api/user/login?turnstile=',
    {
      username: '2265377601@qq.com',
      password: 'current-password',
    },
    { skipAuthRefresh: true }
  )
  expect(useAuthStore.getState().auth.pendingLoginVerification).toMatchObject({
    challenge: { flow_token: 'mfa-challenge' },
    redirectTo: destination,
  })
  expect(useAuthStore.getState().auth.user).toBeNull()
})

it.each([1, 2])(
  'requires an explicit account choice even when only %s password-verified candidate is returned',
  async (count) => {
    const candidates = Array.from({ length: count }, (_, i) => ({
      selection_id: `choice-${i}`,
      username: `user-${i}`,
      account_label: `Account ${i}`,
    }))
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValueOnce({
        data: {
          success: true,
          data: {
            requires_identity_selection: true,
            flow_token: 'private-choice',
            expires_at: Math.floor(Date.now() / 1000) + 300,
            candidates,
          },
        },
      })
      .mockResolvedValueOnce({ data: { success: true, data: challenge() } })
    const { user } = await renderLogin()
    await user.type(screen.getByLabelText('Username or Email'), 'shared-name')
    await user.type(
      screen.getByLabelText('Password', { exact: true }),
      'current-password'
    )
    await user.click(screen.getByRole('button', { name: 'Sign in' }))
    const choose = await screen.findByRole('button', {
      name: `Sign in as user-${count - 1} Account ${count - 1}`,
    })
    expect(post).toHaveBeenCalledTimes(1)
    expect(useAuthStore.getState().auth.user).toBeNull()
    expect(useAuthStore.getState().auth.pendingLoginVerification).toBeNull()
    expect(JSON.stringify(localStorage)).not.toContain('private-choice')
    await user.click(choose)
    await screen.findByText('Complete account verification')
    expect(post).toHaveBeenLastCalledWith(
      '/api/user/login/select',
      {
        flow_token: 'private-choice',
        selection_id: `choice-${count - 1}`,
      },
      { skipAuthRefresh: true, singleUseAuthorization: true }
    )
  }
)

it('cancels account selection without authenticating or retaining the password', async () => {
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      data: {
        requires_identity_selection: true,
        flow_token: 'private-choice',
        expires_at: Math.floor(Date.now() / 1000) + 300,
        candidates: [
          {
            selection_id: 'only-choice',
            username: 'user-a',
            account_label: 'Account A',
          },
        ],
      },
    },
  })
  const { user } = await renderLogin()
  await user.type(screen.getByLabelText('Username or Email'), 'shared-name')
  await user.type(
    screen.getByLabelText('Password', { exact: true }),
    'current-password'
  )
  await user.click(screen.getByRole('button', { name: 'Sign in' }))
  await user.click(
    await screen.findByRole('button', { name: 'Back to sign in' })
  )
  expect(screen.getByLabelText('Password', { exact: true })).toHaveValue('')
  expect(post).toHaveBeenCalledTimes(1)
  expect(useAuthStore.getState().auth.user).toBeNull()
})

it('still requires a fresh verification after a script failure for New API login', async () => {
  client.setQueryData(STATUS_QUERY_KEY, {
    password_login_enabled: true,
    turnstile_check: true,
    turnstile_site_key: 'test-site',
  })
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: false, message: 'Login failed' } })
  const { user } = await renderLogin()
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
  expect(screen.getByRole('button', { name: 'Sign in' })).toBeDisabled()
  await user.click(screen.getByRole('button', { name: 'Sign in' }))
  expect(post).not.toHaveBeenCalled()
  act(() => (options.callback as (token: string) => void)('fresh-token'))
  expect(screen.getByRole('button', { name: 'Sign in' })).toBeEnabled()
  await user.click(screen.getByRole('button', { name: 'Sign in' }))
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith(
      '/api/user/login?turnstile=fresh-token',
      { username: 'existing-user', password: 'existing-password' },
      { skipAuthRefresh: true }
    )
  )
})

it('rejects an expired choice without submitting or creating a session', async () => {
  const now = Date.now()
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      data: {
        requires_identity_selection: true,
        flow_token: 'private-choice',
        expires_at: Math.floor(now / 1000) + 300,
        candidates: [
          {
            selection_id: 'only-choice',
            username: 'user-a',
            account_label: 'Account A',
          },
        ],
      },
    },
  })
  const { user } = await renderLogin()
  await user.type(screen.getByLabelText('Username or Email'), 'shared-name')
  await user.type(
    screen.getByLabelText('Password', { exact: true }),
    'current-password'
  )
  await user.click(screen.getByRole('button', { name: 'Sign in' }))
  const choose = await screen.findByRole('button', {
    name: 'Sign in as user-a Account A',
  })
  vi.spyOn(Date, 'now').mockReturnValue(now + 301000)
  await user.click(choose)
  expect(screen.getByLabelText('Password', { exact: true })).toHaveValue('')
  expect(post).toHaveBeenCalledTimes(1)
  expect(useAuthStore.getState().auth.user).toBeNull()
})
