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
import { defaultParseSearch } from '@tanstack/react-router'
import { afterEach, expect, it, vi } from 'vitest'
import type { z } from 'zod'

import { Route } from '@/routes/(auth)/sign-in'
import { useAuthStore, type AuthBundle } from '@/stores/auth-store'

vi.mock('@/lib/auth-session', () => ({ resolveAuthentication: vi.fn() }))
vi.mock('@/features/auth/sign-in', () => ({ SignIn: () => null }))

const beforeLoad = Route.options.beforeLoad as (context: {
  search: { redirect?: string; qxchat?: string }
}) => Promise<unknown>
const searchSchema = Route.options.validateSearch as z.ZodType<{
  redirect?: string
  qxchat?: '1'
}>

const existingAccount = {
  access_token: 'existing-access',
  token_type: 'Bearer',
  access_expires_at: Math.floor(Date.now() / 1000) + 600,
  user: { id: 1520, username: 'shared-username', role: 1 },
  session: {
    sid: 'existing-session',
    current: true,
    login_method: 'password',
    ip: '127.0.0.1',
    user_agent: 'test',
    created_at: 100,
    last_active_at: 100,
    expires_at: 1000,
  },
} satisfies AuthBundle

afterEach(() => useAuthStore.getState().auth.reset('idle'))

it('requires original-account authentication even if another New API account is already signed in', async () => {
  useAuthStore.getState().auth.setBundle(existingAccount)
  const search = searchSchema.parse(
    defaultParseSearch(
      '?qxchat=1&redirect=%2Fcommunity-authorize%3Fstate%3Dunchanged'
    )
  )
  expect(search.qxchat).toBe('1')
  await expect(
    beforeLoad({
      search,
    })
  ).resolves.toBeUndefined()
  expect(useAuthStore.getState().auth.user?.id).toBe(1520)
})

it('accepts the router-encoded original-account flag and ignores unrelated values', () => {
  expect(searchSchema.parse(defaultParseSearch('?qxchat=%221%22')).qxchat).toBe(
    '1'
  )
  expect(
    searchSchema.parse(defaultParseSearch('?qxchat=true')).qxchat
  ).toBeUndefined()
})

it('retains ordinary signed-in redirects and sanitizes external destinations', async () => {
  useAuthStore.getState().auth.setBundle(existingAccount)
  await expect(
    beforeLoad({ search: { redirect: 'https://untrusted.example/' } })
  ).rejects.toMatchObject({ options: { href: '/dashboard' } })
})
