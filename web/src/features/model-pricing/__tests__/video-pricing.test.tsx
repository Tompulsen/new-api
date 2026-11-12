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
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createRef } from 'react'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import type { BillingUsageSchema } from '@/features/pricing/types'
import {
  ModelPricingEditorPanel,
  type ModelPricingEditorPanelHandle,
} from '@/features/system-settings/models/model-pricing-sheet'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'
import { usePricingPreferencesStore } from '@/stores/pricing-preferences-store'
import { useSystemConfigStore } from '@/stores/system-config-store'

import { ModelPricingPanel } from '../model-pricing-panel'
import { pricingOptions, type PricingValues } from '../pricing'

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
const clients: QueryClient[] = []

beforeEach(() => {
  usePricingPreferencesStore.setState({ currency: 'USD' })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: [], vendors: [] },
  })
})

afterEach(() => {
  cleanup()
  clients.splice(0).forEach((client) => client.clear())
  useAuthStore.getState().auth.setUser(null)
  localStorage.clear()
  vi.restoreAllMocks()
})

function wrapper(props: { children: React.ReactNode }) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  return (
    <QueryClientProvider client={client}>{props.children}</QueryClientProvider>
  )
}

it('offers an independent video mode, requires an explicit price, and keeps the draft across mode switches', async () => {
  const editor = createRef<ModelPricingEditorPanelHandle>()
  const dirty = vi.fn()
  render(
    <ModelPricingEditorPanel
      ref={editor}
      editData={{
        name: 'unbound-video',
        billingMode: 'per-request',
        price: '0.2',
      }}
      videoUsageSchema={schema}
      onDirtyChange={dirty}
    />,
    { wrapper }
  )
  const user = userEvent.setup()
  expect(dirty).toHaveBeenLastCalledWith(false)
  await user.click(screen.getByRole('tab', { name: 'Video billing' }))
  expect(dirty).toHaveBeenLastCalledWith(true)
  expect(await editor.current?.commitDraft()).toBeNull()
  const price = screen.getByRole('textbox', {
    name: /Video generation unit price:.*480p/,
  })
  await user.clear(price)
  await user.type(price, '0.25')
  await user.click(
    screen.getByRole('tab', { name: 'Per-request (deprecated)' })
  )
  await user.click(screen.getByRole('tab', { name: 'Video billing' }))
  expect(
    screen.getByRole('textbox', {
      name: /Video generation unit price:.*480p/,
    })
  ).toHaveValue('0.25')
  const draft = await editor.current?.commitDraft()
  expect(draft?.billingMode).toBe('video')
  expect(draft?.pluginBillingExpr?.sora).toContain('0.25')
})

it.each([
  {},
  { ModelPrice: 0.2, 'billing_setting.billing_mode': 'ratio' },
  {
    'billing_setting.plugin_billing_expr': { xai: 'tier("base", 0.5)' },
  },
] satisfies PricingValues[])(
  'saves a video price without changing the original ordinary or other-provider prices (%j)',
  async (configured) => {
    useAuthStore.getState().auth.setUser({
      id: 1,
      username: 'administrator',
      role: 100,
    })
    vi.spyOn(api, 'get').mockImplementation(async (url) => ({
      data: {
        success: true,
        data:
          url === '/api/option/model_pricing'
            ? {
                entries: [
                  {
                    model_name: 'example',
                    version: 'v1',
                    configured,
                    effective: {
                      ...configured,
                      'billing_setting.billing_mode': 'tiered_expr',
                      'billing_setting.billing_expr':
                        'tier("builtin", p * 2 + c * 3)',
                    },
                  },
                ],
                options: pricingOptions({}),
                empty_version: 'empty',
                video_usage_schema: schema,
              }
            : [],
        vendors: [],
      },
    }))
    const patch = vi
      .spyOn(api, 'patch')
      .mockResolvedValue({ data: { success: true } })
    render(<ModelPricingPanel modelName='example' />, { wrapper })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('tab', { name: 'Video billing' }))
    const price = screen.getByRole('textbox', {
      name: /Video generation unit price:.*480p/,
    })
    await user.clear(price)
    await user.type(price, '0.1')
    await user.click(screen.getByRole('button', { name: 'Save model prices' }))
    await waitFor(() => expect(patch).toHaveBeenCalled())
    const request = patch.mock.calls[0][1] as {
      changes: { pricing: PricingValues }[]
    }
    const saved = request.changes[0].pricing
    expect(saved).toEqual({
      ...configured,
      'billing_setting.plugin_billing_expr': {
        ...configured['billing_setting.plugin_billing_expr'],
        sora: expect.stringContaining('0.1'),
      },
    })
  }
)

it('reopens the stored video price and removes only that provider when saving an ordinary mode', async () => {
  const editor = createRef<ModelPricingEditorPanelHandle>()
  render(
    <ModelPricingEditorPanel
      ref={editor}
      editData={{
        name: 'saved-video',
        billingMode: 'per-request',
        price: '0.2',
        pluginBillingExpr: {
          sora: 'tier("base", 0.3)',
          xai: 'tier("base", 0.5)',
        },
      }}
      videoUsageSchema={schema}
    />,
    { wrapper }
  )
  expect(screen.getByRole('tab', { name: 'Video billing' })).toHaveAttribute(
    'aria-selected',
    'true'
  )
  expect(await screen.findByLabelText('Per request (USD)')).toHaveValue('0.3')
  const user = userEvent.setup()
  await user.click(
    screen.getByRole('tab', { name: 'Per-request (deprecated)' })
  )
  const draft = await editor.current?.commitDraft()
  expect(draft?.pluginBillingExpr).toEqual({ xai: 'tier("base", 0.5)' })
  expect(draft?.price).toBe('0.2')
})

it('commits the edited raw video price and request rule together', async () => {
  const editor = createRef<ModelPricingEditorPanelHandle>()
  render(
    <ModelPricingEditorPanel
      ref={editor}
      editData={{
        name: 'raw-video',
        pluginBillingExpr: { sora: 'tier("base", u("seconds") * 0.1)' },
      }}
      videoUsageSchema={schema}
    />,
    { wrapper }
  )
  const user = userEvent.setup()
  await user.click(screen.getByRole('combobox', { name: 'Editor mode' }))
  await user.click(screen.getByRole('option', { name: 'Expression editor' }))
  const expression =
    '(tier("base", u("seconds") * 0.7)) * (header("x-plan") == "fast" ? 2 : 1)'
  fireEvent.change(
    screen.getByRole('textbox', { name: 'Billing expression' }),
    {
      target: { value: expression },
    }
  )
  const draft = await editor.current?.commitDraft()
  expect(draft?.pluginBillingExpr?.sora).toBe(expression)
})

it('labels per-request pricing with the selected currency and preserves the USD price', async () => {
  const currency = useSystemConfigStore.getState().config.currency
  useSystemConfigStore.setState((state) => ({
    config: {
      ...state.config,
      currency: { ...currency, quotaDisplayType: 'CNY', usdExchangeRate: 7 },
    },
  }))
  usePricingPreferencesStore.setState({ currency: 'site' })
  const editor = createRef<ModelPricingEditorPanelHandle>()
  try {
    render(
      <ModelPricingEditorPanel
        ref={editor}
        editData={{
          name: 'cny-video',
          pluginBillingExpr: { sora: 'tier("base", 0.3)' },
        }}
        videoUsageSchema={schema}
      />,
      { wrapper }
    )
    expect(screen.getByLabelText('Per request (CNY)')).toHaveValue('2.1')
    const draft = await editor.current?.commitDraft()
    expect(draft?.pluginBillingExpr?.sora).toBe('tier("base", 0.3)')
  } finally {
    useSystemConfigStore.setState((state) => ({
      config: { ...state.config, currency },
    }))
  }
})

it('preserves saved video prices when the video plugin is unavailable', async () => {
  const editor = createRef<ModelPricingEditorPanelHandle>()
  render(
    <ModelPricingEditorPanel
      ref={editor}
      editData={{
        name: 'saved-video',
        billingMode: 'per-request',
        price: '0.2',
        pluginBillingExpr: { sora: 'tier("base", 0.3)' },
      }}
    />,
    { wrapper }
  )
  expect(
    screen.queryByRole('tab', { name: 'Video billing' })
  ).not.toBeInTheDocument()
  const draft = await editor.current?.commitDraft()
  expect(draft?.pluginBillingExpr).toEqual({ sora: 'tier("base", 0.3)' })
})

it('keeps an explicit zero video price when saved', async () => {
  const editor = createRef<ModelPricingEditorPanelHandle>()
  render(
    <ModelPricingEditorPanel
      ref={editor}
      editData={{
        name: 'free-video',
        price: '0.2',
        billingMode: 'per-request',
      }}
      videoUsageSchema={schema}
    />,
    { wrapper }
  )
  const user = userEvent.setup()
  await user.click(screen.getByRole('tab', { name: 'Video billing' }))
  await user.click(screen.getByRole('tab', { name: 'Per request' }))
  await user.type(screen.getByLabelText('Per request (USD)'), '0')
  const draft = await editor.current?.commitDraft()
  expect(draft?.pluginBillingExpr?.sora).toBe('tier("base", 0)')
})

it('resets the video draft when switching to a different model', async () => {
  const editor = createRef<ModelPricingEditorPanelHandle>()
  const view = render(
    <ModelPricingEditorPanel
      ref={editor}
      editData={{
        name: 'video-a',
        pluginBillingExpr: { sora: 'tier("base", 0.3)' },
      }}
      videoUsageSchema={schema}
    />,
    { wrapper }
  )
  view.rerender(
    <ModelPricingEditorPanel
      ref={editor}
      editData={{ name: 'video-b', price: '0.2', billingMode: 'per-request' }}
      videoUsageSchema={schema}
    />
  )
  const user = userEvent.setup()
  await user.click(screen.getByRole('tab', { name: 'Video billing' }))
  expect(await editor.current?.commitDraft()).toBeNull()
})

it('preserves native Sora size prices without treating them as generic video prices', async () => {
  const editor = createRef<ModelPricingEditorPanelHandle>()
  const expression =
    'u("size") == "1280x720" ? tier("720p", u("seconds") * 0.1) : tier("other", u("seconds") * 0.2)'
  render(
    <ModelPricingEditorPanel
      ref={editor}
      editData={{
        name: 'sora-native',
        billingMode: 'per-request',
        price: '0.2',
        pluginBillingExpr: { sora: expression },
      }}
      videoUsageSchema={schema}
    />,
    { wrapper }
  )
  expect(screen.getByRole('tab', { name: 'Video billing' })).toHaveAttribute(
    'aria-selected',
    'false'
  )
  const draft = await editor.current?.commitDraft()
  expect(draft?.pluginBillingExpr?.sora).toBe(expression)
})

it.each([
  ['async-compatible', 'video'],
  ['async-compatible', 'ordinary'],
  ['sd-compatible', 'video'],
  ['sd-compatible', 'ordinary'],
])(
  'edits %s and preserves the separate Sora price when saving %s mode',
  async (pluginKey, mode) => {
    const editor = createRef<ModelPricingEditorPanelHandle>()
    const soraPrice = 'tier("sora", u("seconds") * 0.8)'
    render(
      <ModelPricingEditorPanel
        ref={editor}
        editData={{
          name: 'seedance-2.0-fast-official',
          billingMode: 'per-request',
          price: '0.2',
          pluginBillingExpr: {
            [pluginKey]: 'tier("async", u("seconds") * 0.4)',
            sora: soraPrice,
          },
        }}
        pluginVariants={[
          {
            plugin_key: pluginKey,
            plugin_name: 'Async compatible',
            usage_schema: schema,
            configured: 'tier("async", u("seconds") * 0.4)',
            effective: 'tier("async", u("seconds") * 0.4)',
            compatible: true,
          },
          {
            plugin_key: 'sora',
            plugin_name: 'Sora',
            usage_schema: schema,
            configured: soraPrice,
            effective: soraPrice,
            compatible: true,
          },
        ]}
        videoUsageSchema={schema}
      />,
      { wrapper }
    )
    expect(screen.getByRole('tab', { name: 'Video billing' })).toHaveAttribute(
      'aria-selected',
      'true'
    )
    const user = userEvent.setup()
    const price = screen.getByRole('textbox', {
      name: /Video generation unit price:.*720p/,
    })
    expect(price).toHaveValue('0.4')
    if (mode === 'video') {
      await user.clear(price)
      await user.type(price, '0.6')
    } else {
      await user.click(
        screen.getByRole('tab', { name: 'Per-request (deprecated)' })
      )
    }
    const draft = await editor.current?.commitDraft()
    expect(draft?.pluginBillingExpr?.sora).toBe(soraPrice)
    if (mode === 'video') {
      expect(draft?.pluginBillingExpr?.[pluginKey]).toContain('0.6')
    } else {
      expect(draft?.pluginBillingExpr).not.toHaveProperty(pluginKey)
    }
  }
)

it.each(['async-compatible', 'sd-compatible'])(
  'creates a %s video price without inheriting or replacing an existing Sora override',
  async (pluginKey) => {
    const editor = createRef<ModelPricingEditorPanelHandle>()
    const soraPrice = 'tier("sora", u("seconds") * 0.8)'
    render(
      <ModelPricingEditorPanel
        ref={editor}
        editData={{
          name: 'seedance-2.0-fast-official',
          billingMode: 'per-request',
          price: '0.2',
          pluginBillingExpr: { sora: soraPrice },
        }}
        pluginVariants={[
          {
            plugin_key: pluginKey,
            plugin_name: 'Async compatible',
            usage_schema: schema,
            configured: '',
            effective: '',
            compatible: false,
          },
        ]}
        videoUsageSchema={schema}
      />,
      { wrapper }
    )
    const user = userEvent.setup()
    await user.click(screen.getByRole('tab', { name: 'Video billing' }))
    expect(await editor.current?.commitDraft()).toBeNull()
    await user.click(screen.getByRole('tab', { name: 'Per request' }))
    await user.type(screen.getByLabelText('Per request (USD)'), '0.3')
    expect((await editor.current?.commitDraft())?.pluginBillingExpr).toEqual({
      sora: soraPrice,
      [pluginKey]: 'tier("base", 0.3)',
    })
  }
)
