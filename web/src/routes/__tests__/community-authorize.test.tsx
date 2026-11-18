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
  defaultParseSearch,
  Outlet,
  RouterProvider,
} from '@tanstack/react-router'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'

import type { CommunitySsoConfig } from '@/features/auth/lib/community-sso-search'
import { api } from '@/lib/api'
import { Route } from '@/routes/community-authorize'
import { useAuthStore } from '@/stores/auth-store'

let client: QueryClient
afterEach(() => {
  cleanup()
  client?.clear()
  vi.restoreAllMocks()
  useAuthStore.getState().auth.reset('idle')
})

async function renderAuthorization(
  profileKind: 'independent' | 'original' | 'error' = 'independent',
  qxchat = true,
  profileWait?: Promise<void>
) {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  useAuthStore.getState().auth.setUser({
    id: 690,
    username: 'native-account',
    display_name: 'Shared display name',
    role: 1,
  })
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/user/qxchat') {
      await profileWait
      if (profileKind === 'error') throw new Error('unavailable')
      return {
        data: {
          success: true,
          data: {
            enabled: true,
            state: 'active',
            profile_kind: profileKind,
            username: 'legacy-account',
          },
        },
      }
    }
    return {
      data: {
        enabled: true,
        community_origin: 'https://qx.example',
        studio_origin: '',
        community_origins: ['https://qx.example'],
        qxchat_origins: qxchat ? ['https://qx.example'] : [],
      },
    }
  })
  const search = new URLSearchParams({
    client_id: 'https://qx.example',
    redirect_uri: 'https://qx.example/api/newapi/callback',
    response_type: 'code',
    scope: 'identity',
    state: 's'.repeat(43),
    code_challenge: 'c'.repeat(43),
    code_challenge_method: 'S256',
  })
  const destination = `/community-authorize?${search}`
  const root = createRootRoute({ component: Outlet })
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({
        getParentRoute: () => root,
        path: '/community-authorize',
        component: Route.options.component,
        loader: Route.options.loader as () => Promise<CommunitySsoConfig>,
        validateSearch: Route.options.validateSearch,
      }),
      createRoute({
        getParentRoute: () => root,
        path: '/sign-in',
        component: () => <p>Original account login</p>,
      }),
      createRoute({
        getParentRoute: () => root,
        path: '/dashboard',
        component: () => <p>Account center</p>,
      }),
    ]),
    history: createMemoryHistory({ initialEntries: [destination] }),
  })
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
  return { router, destination, user: userEvent.setup() }
}

it('identifies an independent QXChat profile before authorizing and preserves the original login destination', async () => {
  const { router, destination, user } = await renderAuthorization()
  expect(
    await screen.findByRole('button', {
      name: 'Continue with this separate profile',
    })
  ).toBeEnabled()
  expect(screen.getByText('native-account · #690')).toBeVisible()
  await user.click(
    screen.getByRole('button', { name: 'Use my original QXChat account' })
  )
  await waitFor(() => expect(router.state.location.pathname).toBe('/sign-in'))
  const redirected = new URL(
    router.state.location.href,
    'https://newapi.example'
  )
  const signInSearch = defaultParseSearch(redirected.search) as Record<
    string,
    unknown
  >
  expect(signInSearch.qxchat).toBe('1')
  const redirect = signInSearch.redirect as string
  expect(
    new URL(redirect, 'https://newapi.example').searchParams.get('state')
  ).toBe('s'.repeat(43))
  expect(
    new URL(redirect, 'https://newapi.example').searchParams.get(
      'code_challenge'
    )
  ).toBe('c'.repeat(43))
  expect(
    new URL(redirect, 'https://newapi.example').searchParams.get('redirect_uri')
  ).toBe('https://qx.example/api/newapi/callback')
  expect(destination).toContain('code_challenge_method=S256')
})

it('waits for the profile check and explicit confirmation before authorizing the separate profile', async () => {
  let finishProfileCheck: () => void = () => {}
  const profileWait = new Promise<void>((resolve) => {
    finishProfileCheck = resolve
  })
  vi.spyOn(api, 'post').mockRejectedValue(new Error('unavailable'))
  const { user } = await renderAuthorization('independent', true, profileWait)
  expect(await screen.findByText('Checking your QXChat profile…')).toBeVisible()
  expect(
    screen.getByRole('button', { name: 'Continue with this account' })
  ).toBeDisabled()
  finishProfileCheck()
  const confirm = await screen.findByRole('button', {
    name: 'Continue with this separate profile',
  })
  expect(confirm).toBeEnabled()
  expect(api.post).not.toHaveBeenCalled()
  await user.click(confirm)
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Could not continue. Please try again.'
  )
  expect(api.post).toHaveBeenCalledWith(
    '/api/community-sso/authorize',
    expect.objectContaining({
      client_id: 'https://qx.example',
      redirect_uri: 'https://qx.example/api/newapi/callback',
      state: 's'.repeat(43),
      code_challenge: 'c'.repeat(43),
      code_challenge_method: 'S256',
    }),
    expect.any(Object)
  )
})

it('shows the linked original profile without labeling its points as a new separate balance', async () => {
  await renderAuthorization('original')
  expect(
    await screen.findByText('Original QXChat profile: legacy-account')
  ).toBeVisible()
  expect(
    screen.getByRole('button', { name: 'Continue with this account' })
  ).toBeEnabled()
  expect(
    screen.queryByRole('button', {
      name: 'Continue with this separate profile',
    })
  ).not.toBeInTheDocument()
})

it('does not authorize when the QXChat profile check fails and offers a retry', async () => {
  await renderAuthorization('error')
  expect(await screen.findByRole('button', { name: 'Retry' })).toBeEnabled()
  expect(
    screen.getByRole('button', { name: 'Continue with this account' })
  ).toBeDisabled()
})

it('keeps community authorization separate from the QXChat account choice', async () => {
  await renderAuthorization('independent', false)
  expect(
    await screen.findByRole('button', { name: 'Continue with this account' })
  ).toBeEnabled()
  expect(
    screen.queryByRole('button', { name: 'Use my original QXChat account' })
  ).not.toBeInTheDocument()
  expect(api.get).not.toHaveBeenCalledWith(
    '/api/user/qxchat',
    expect.anything()
  )
})
