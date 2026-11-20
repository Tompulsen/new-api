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
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { AffiliateRewardsCard } from '../components/affiliate-rewards-card'
import { useAffiliate } from '../hooks/use-affiliate'
import type { UserWalletData } from '../types'

const wallet: UserWalletData = {
  id: 1,
  username: 'inviter',
  quota: 0,
  used_quota: 0,
  request_count: 0,
  aff_quota: 1000,
  aff_history_quota: 2000,
  aff_count: 9,
  group: 'default',
}
const overview = {
  config: {
    version: 1,
    enabled: true,
    inviterPoints: 5000,
    inviteePoints: 5000,
    inviterQuota: 500000,
    inviteeQuota: 500000,
    minTopupCny: 10.25,
    updatedAt: 0,
  },
  code: 'Ab12',
  invited: 12,
  credited: 2,
  waiting: 4,
  pending: 1,
  earnedPoints: 10000,
  earnedQuota: 1000000,
  legacyInvited: 5,
  asInvitee: null,
  items: [],
  nextCursor: 0,
}
let client: QueryClient

function Fixture() {
  const affiliate = useAffiliate()
  return (
    <AffiliateRewardsCard
      user={wallet}
      affiliateLink={affiliate.affiliateLink}
      legacy={affiliate.legacy}
      overview={affiliate.overview}
      loading={affiliate.loading}
      error={affiliate.error}
      onRetry={() => void affiliate.refetch()}
      onTransfer={vi.fn()}
    />
  )
}

function renderRewards() {
  render(
    <QueryClientProvider client={client}>
      <Fixture />
    </QueryClientProvider>
  )
}

beforeEach(() => {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  useAuthStore.getState().auth.setUser({ id: 1, username: 'inviter', role: 1 })
})
afterEach(() => {
  cleanup()
  client.clear()
  vi.restoreAllMocks()
  useAuthStore.getState().auth.reset('idle')
})

test('unified referral statistics use the account-center overview and keep the local registration origin', async () => {
  const get = vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data: url === '/api/user/self/wallet' ? { enabled: true } : overview,
    },
  }))
  renderRewards()
  expect(await screen.findByText('Awaiting qualifying top-up')).toBeVisible()
  expect(screen.getByText('4')).toBeVisible()
  expect(screen.getByText('12')).toBeVisible()
  expect(screen.getByText('2')).toBeVisible()
  expect(screen.queryByText('9')).not.toBeInTheDocument()
  expect(screen.getByText('Minimum qualifying top-up: 10.25 CNY')).toBeVisible()
  expect(screen.getByRole('textbox', { name: 'Referral link' })).toHaveValue(
    `${window.location.origin}/sign-up?aff=Ab12`
  )
  expect(get).toHaveBeenCalledWith(
    '/api/user/self/referrals',
    expect.anything()
  )
  expect(
    screen.getByRole('button', { name: 'Transfer previous rewards' })
  ).toBeVisible()
})

test('an unavailable overview shows retry instead of successful zero statistics', async () => {
  let unavailable = true
  const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/user/self/wallet') {
      return { data: { success: true, data: { enabled: true } } }
    }
    if (unavailable) throw new Error('Unavailable')
    return { data: { success: true, data: overview } }
  })
  renderRewards()
  expect(
    await screen.findByText('Referral rewards are unavailable')
  ).toBeVisible()
  expect(screen.queryByText('Total Earned')).not.toBeInTheDocument()
  expect(get).not.toHaveBeenCalledWith('/api/user/aff', expect.anything())
  unavailable = false
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Retry' }))
  expect(await screen.findByText('Awaiting qualifying top-up')).toBeVisible()
})

test('changing accounts removes the previous referral code and statistics while loading', async () => {
  const get = vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data: url === '/api/user/self/wallet' ? { enabled: true } : overview,
    },
  }))
  renderRewards()
  expect(
    await screen.findByRole('textbox', { name: 'Referral link' })
  ).toHaveValue(`${window.location.origin}/sign-up?aff=Ab12`)
  get.mockReturnValue(new Promise(() => {}))
  act(() =>
    useAuthStore.getState().auth.setUser({ id: 2, username: 'other', role: 1 })
  )
  expect(
    screen.queryByRole('textbox', { name: 'Referral link' })
  ).not.toBeInTheDocument()
  expect(screen.queryByText('12')).not.toBeInTheDocument()
})

test('explicitly disabled unified wallet preserves native invitations and transfers', async () => {
  const get = vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data: url === '/api/user/self/wallet' ? { enabled: false } : 'LegacyAB',
    },
  }))
  renderRewards()
  expect(
    await screen.findByRole('textbox', { name: 'Referral link' })
  ).toHaveValue(`${window.location.origin}/sign-up?aff=LegacyAB`)
  expect(screen.getByText('9')).toBeVisible()
  expect(screen.getByText('Pending')).toBeVisible()
  expect(
    screen.getByText(
      'Earn rewards when users join through your referral link. Transfer accumulated rewards to your balance anytime.'
    )
  ).toBeVisible()
  expect(
    screen.getByRole('button', { name: 'Transfer to Balance' })
  ).toBeVisible()
  expect(
    screen.queryByText('Awaiting qualifying top-up')
  ).not.toBeInTheDocument()
  expect(get).toHaveBeenCalledWith('/api/user/aff', expect.anything())
  expect(get).not.toHaveBeenCalledWith(
    '/api/user/self/referrals',
    expect.anything()
  )
})

test('wallet failure never falls back to native referral data', async () => {
  const get = vi.spyOn(api, 'get').mockRejectedValue(new Error('Unavailable'))
  renderRewards()
  expect(
    await screen.findByText('Referral rewards are unavailable')
  ).toBeVisible()
  expect(
    screen.queryByRole('textbox', { name: 'Referral link' })
  ).not.toBeInTheDocument()
  expect(screen.queryByText('9')).not.toBeInTheDocument()
  expect(get).toHaveBeenCalledWith('/api/user/self/wallet', expect.anything())
  expect(get).not.toHaveBeenCalledWith('/api/user/aff', expect.anything())
  expect(get).not.toHaveBeenCalledWith(
    '/api/user/self/referrals',
    expect.anything()
  )
})

test('threshold formatting follows every supported interface language', async () => {
  const languages = [
    ['en', '10.25'],
    ['zhCN', '10.25'],
    ['zhTW', '10.25'],
    ['fr', '10,25'],
    ['ru', '10,25'],
    ['ja', '10.25'],
    ['vi', '10,25'],
  ]
  const key = 'Minimum qualifying top-up: {{amount}} CNY'
  const i18n = createInstance()
  await i18n.init({
    lng: 'en',
    fallbackLng: 'en',
    resources: Object.fromEntries(
      languages.map(([language]) => [language, { translation: { [key]: key } }])
    ),
  })
  render(
    <I18nextProvider i18n={i18n}>
      <AffiliateRewardsCard
        user={wallet}
        overview={overview}
        affiliateLink='https://example.test/sign-up?aff=Ab12'
        onTransfer={vi.fn()}
        onRetry={vi.fn()}
      />
    </I18nextProvider>
  )

  for (const [language, amount] of languages) {
    await act(() => i18n.changeLanguage(language))
    expect(
      screen.getByText(`Minimum qualifying top-up: ${amount} CNY`)
    ).toBeVisible()
  }
})

test('a zero threshold still requires payment and current rewards need no manual transfer', () => {
  render(
    <AffiliateRewardsCard
      user={{ ...wallet, aff_quota: 0 }}
      overview={{
        ...overview,
        config: { ...overview.config, enabled: false, minTopupCny: 0 },
      }}
      affiliateLink='https://example.test/sign-up?aff=Ab12'
      onTransfer={vi.fn()}
      onRetry={vi.fn()}
    />
  )
  expect(
    screen.getByText('Any positive verified payment qualifies.')
  ).toBeVisible()
  expect(screen.getByText('New referral rewards are paused.')).toBeVisible()
  expect(
    screen.queryByRole('button', { name: 'Transfer previous rewards' })
  ).not.toBeInTheDocument()
})
