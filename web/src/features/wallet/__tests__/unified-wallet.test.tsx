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
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider, initReactI18next } from 'react-i18next'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { UnifiedWalletCard } from '../components/unified-wallet-card'
import { formatCommunityPoints } from '../lib/format'

let client: QueryClient
beforeEach(() => {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  useAuthStore.getState().auth.setUser({ id: 1, username: 'wallet-user', role: 1 })
})
afterEach(() => {
  cleanup()
  client.clear()
  vi.restoreAllMocks()
  useAuthStore.getState().auth.reset('idle')
})

const wallet = { enabled: true, currency: 'CNY', points_per_cny: 5000, charity_points: '100.000001', permanent_points: '2500', charity_expiring_at: null, legacy_api_quota: 20, paid_api_quota: 50, total_api_quota: 70 }

it('displays separate charity and permanent balances and changes formatting with the interface language', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({ data: { success: true, data: wallet } })
  const i18n = createInstance()
  await i18n.use(initReactI18next).init({ lng: 'en', resources: { en: { translation: {} }, fr: { translation: {} } }, fallbackLng: 'en' })
  render(<QueryClientProvider client={client}><I18nextProvider i18n={i18n}><UnifiedWalletCard /></I18nextProvider></QueryClientProvider>)
  expect(await screen.findByText('100.000001')).toBeVisible()
  expect(screen.getByText('2,500')).toBeVisible()
  expect(screen.getByText('Only available in the community. Original expiry dates are preserved.')).toBeVisible()
  expect(screen.getByText('The community view of your paid New API balance.')).toBeVisible()
  await act(() => i18n.changeLanguage('fr'))
  expect(screen.getByText('100,000001')).toBeVisible()
})

it('hides balances when disabled and does not use a previous account balance for a new session', async () => {
  const get = vi.spyOn(api, 'get').mockResolvedValue({ data: { success: true, data: { ...wallet, enabled: false } } })
  render(<QueryClientProvider client={client}><UnifiedWalletCard /></QueryClientProvider>)
  await waitFor(() => expect(client.getQueryData(['unified-wallet', 1, undefined])).toEqual({ ...wallet, enabled: false }))
  expect(screen.queryByText('Charity points')).not.toBeInTheDocument()
  get.mockResolvedValue({ data: { success: true, data: { ...wallet, charity_points: '42' } } })
  act(() => useAuthStore.getState().auth.setUser({ id: 2, username: 'other-wallet', role: 1 }))
  expect(await screen.findByText('42')).toBeVisible()
  expect(screen.queryByText('100.000001')).not.toBeInTheDocument()
})

it('shows a retry control on failure and refreshes balances after retry', async () => {
  const get = vi.spyOn(api, 'get').mockRejectedValueOnce(new Error('Unavailable'))
  render(<QueryClientProvider client={client}><UnifiedWalletCard /></QueryClientProvider>)
  expect(await screen.findByText('Community points are unavailable')).toBeVisible()
  get.mockResolvedValue({ data: { success: true, data: wallet } })
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Retry' }))
  expect(await screen.findByText('100.000001')).toBeVisible()
  get.mockResolvedValue({ data: { success: true, data: { ...wallet, charity_points: '99' } } })
  await user.click(screen.getByRole('button', { name: 'Refresh community points' }))
  expect(await screen.findByText('99')).toBeVisible()
})

it.each([
  ['en', '1,234.000001'], ['zhCN', '1,234.000001'], ['zhTW', '1,234.000001'],
  ['fr', '1\u202f234,000001'], ['ru', '1\u00a0234,000001'], ['ja', '1,234.000001'], ['vi', '1.234,000001'],
])('preserves six decimal places for %s', (language, expected) => {
  expect(formatCommunityPoints('1234.000001', language)).toBe(expected)
})

it('keeps exact large decimal balances and safely rejects malformed values', () => {
  expect(formatCommunityPoints('9007199254740991.000001', 'en')).toBe('9,007,199,254,740,991.000001')
  expect(formatCommunityPoints('1234.000001', 'not_a_locale')).not.toBe('-')
  expect(formatCommunityPoints('9007199254740992', 'en')).toBe('-')
  expect(formatCommunityPoints('NaN', 'en')).toBe('-')
})
