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
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'

import type { ModelPricingConfig } from '@/features/model-pricing/api'
import {
  evaluateTaskVisualConfig,
  tryParseTaskVisualConfig,
} from '@/features/pricing/lib/task-expr'
import type { BillingUsageSchema } from '@/features/pricing/types'
import { api } from '@/lib/api'

import { videoPricingChanges } from '../video-billing'
import { VideoBillingSection } from '../video-billing-section'

const schema: BillingUsageSchema = {
  seconds: {
    type: 'number',
    unit: 'second',
    description: { en: 'Video generation unit price' },
  },
  resolution: {
    enum: ['480p', '720p', '1080p'],
    description: { en: 'Output video resolution' },
  },
}
const emptyPricing = {
  entries: [
    {
      model_name: 'new-video',
      version: 'v-new',
      configured: {},
      effective: {},
    },
    {
      model_name: 'priced-video',
      version: 'v-priced',
      configured: { ModelPrice: 0.3 },
      effective: {},
    },
  ],
  options: {},
  empty_version: 'empty',
} as ModelPricingConfig

afterEach(() => vi.restoreAllMocks())

it('generates the selected per-second prices for each resolution', async () => {
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data:
        url === '/api/option/video_billing'
          ? {
              bindings: { channel_ids: [], models: ['new-video'] },
              version: 'v1',
              channels: [],
              models: ['new-video'],
              usage_schema: schema,
            }
          : emptyPricing,
    },
  }))
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: { effective: {} } } })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <VideoBillingSection />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  for (const [resolution, price] of [
    ['480p', '0.05'],
    ['720p', '0.075'],
    ['1080p', '0.1'],
  ]) {
    await user.type(
      await screen.findByLabelText(`${resolution} (USD/second)`),
      price
    )
  }
  await user.click(screen.getByRole('button', { name: 'Review video prices' }))
  await screen.findByRole('alertdialog')
  const request = post.mock.calls[0][1] as {
    pricing: { 'billing_setting.plugin_billing_expr': { sora: string } }
  }
  const parsed = tryParseTaskVisualConfig(
    request.pricing['billing_setting.plugin_billing_expr'].sora,
    schema
  )
  expect(parsed).not.toBeNull()
  if (!parsed) {
    throw new Error('Generated video prices must be representable')
  }
  for (const [resolution, total] of [
    ['480p', 0.3],
    ['720p', 0.45],
    ['1080p', 0.6],
  ] as const) {
    expect(
      evaluateTaskVisualConfig(parsed, { seconds: 6, resolution }, schema)
        ?.total
    ).toBeCloseTo(total, 10)
  }
})

it('preserves existing prices by default and retains other provider overrides when replacement is selected', () => {
  const snapshot = structuredClone(emptyPricing)
  snapshot.entries[1].configured['billing_setting.plugin_billing_expr'] = {
    xai: 'tier("base", 0.2)',
  }
  const original = structuredClone(snapshot)
  const result = videoPricingChanges(
    snapshot,
    ['new-video', 'priced-video', 'new-video'],
    'tier("base", 0.3)',
    false
  )
  expect(result.changes.map((change) => change.model_name)).toEqual([
    'new-video',
  ])
  expect(result.skipped).toEqual(['priced-video'])
  const replacement = videoPricingChanges(
    snapshot,
    ['priced-video'],
    'tier("base", 0.3)',
    true
  )
  expect(replacement.changes[0]).toEqual({
    model_name: 'priced-video',
    expected_version: 'v-priced',
    pricing: {
      ModelPrice: 0.3,
      'billing_setting.plugin_billing_expr': {
        xai: 'tier("base", 0.2)',
        sora: 'tier("base", 0.3)',
      },
    },
  })
  expect(snapshot).toEqual(original)
  expect(() =>
    videoPricingChanges(snapshot, ['missing'], 'tier("base", 0.3)', false)
  ).toThrow()
})

it('reviews a flat task price and saves only unpriced models after confirmation', async () => {
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/option/video_billing') {
      return {
        data: {
          success: true,
          data: {
            bindings: { channel_ids: [354], models: [] },
            version: 'bindings-v1',
            channels: [
              {
                id: 354,
                name: 'Video channel',
                models: ['new-video', 'priced-video'],
              },
            ],
            models: ['new-video', 'priced-video'],
            usage_schema: schema,
          },
        },
      }
    }
    if (url === '/api/option/model_pricing') {
      return { data: { success: true, data: emptyPricing } }
    }
    return { data: { success: true, data: [] } }
  })
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: { effective: {} } } })
  const patch = vi
    .spyOn(api, 'patch')
    .mockResolvedValue({ data: { success: true } })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <VideoBillingSection />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  await screen.findByRole('tab', { name: 'Per request' })
  expect(
    screen.getByRole('switch', {
      name: 'Replace existing prices for selected models',
    })
  ).not.toBeChecked()
  await user.click(screen.getByRole('tab', { name: 'Per request' }))
  await user.type(screen.getByLabelText('USD per request'), '0.3')
  await user.click(screen.getByRole('button', { name: 'Review video prices' }))
  const dialog = await screen.findByRole('alertdialog')
  expect(
    within(dialog).getByText('Apply to 1 models; preserve 1 existing prices.')
  ).toBeVisible()
  expect(patch).not.toHaveBeenCalled()
  await user.click(
    within(dialog).getByRole('button', { name: 'Save model prices' })
  )
  await waitFor(() =>
    expect(patch).toHaveBeenCalledWith('/api/option/model_pricing', {
      changes: [
        {
          model_name: 'new-video',
          expected_version: 'v-new',
          pricing: {
            'billing_setting.plugin_billing_expr': {
              sora: 'tier("base", 0.3)',
            },
          },
        },
      ],
    })
  )
  expect(post).toHaveBeenCalledWith(
    '/api/option/model_pricing/preview',
    expect.objectContaining({ model_name: 'new-video' })
  )
})

it('keeps the confirmation open when a concurrent price edit rejects the batch', async () => {
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data:
        url === '/api/option/video_billing'
          ? {
              bindings: { channel_ids: [], models: ['new-video'] },
              version: 'v1',
              channels: [],
              models: ['new-video'],
              usage_schema: schema,
            }
          : emptyPricing,
    },
  }))
  vi.spyOn(api, 'post').mockResolvedValue({
    data: { success: true, data: { effective: {} } },
  })
  vi.spyOn(api, 'patch').mockRejectedValue(
    new Error('model pricing changed; reload before saving')
  )
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <VideoBillingSection />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  await user.click(await screen.findByRole('tab', { name: 'Per request' }))
  await user.type(screen.getByLabelText('USD per request'), '0.3')
  await user.click(screen.getByRole('button', { name: 'Review video prices' }))
  const dialog = await screen.findByRole('alertdialog')
  await user.click(
    within(dialog).getByRole('button', { name: 'Save model prices' })
  )
  expect(await within(dialog).findByRole('alert')).toHaveTextContent(
    'Failed to save model pricing'
  )
  expect(dialog).toBeVisible()
})
