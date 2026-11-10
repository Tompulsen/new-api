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
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { ModelRatioVisualEditor } from '../model-ratio-visual-editor'
import type { ModelPricingIssue } from '../pricing-issues'

let client: QueryClient | undefined

afterEach(() => {
  client?.clear()
  localStorage.clear()
  window.getSelection()?.removeAllRanges()
  vi.restoreAllMocks()
  useAuthStore.getState().auth.reset()
})

type EditorFixture = {
  issues?: ModelPricingIssue[]
  taskModels?: string[]
  billingMode?: string
  billingExpr?: string
  pluginBillingExpr?: string
  filterMode?: 'all' | 'unset'
  candidateModelNames?: string[]
}

async function renderEditor(fixture: EditorFixture = {}) {
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/pricing') {
      const data = (fixture.taskModels ?? []).map((name) => ({
        model_name: name,
        billing_usage_schema: { seconds: { type: 'number', unit: 'second' } },
      }))
      return { data: { success: true, data, vendors: [] } }
    }
    if (url === '/api/option/model_pricing/issues') {
      return { data: { success: true, data: fixture.issues ?? [] } }
    }
    return { data: { success: true, data: {} } }
  })
  if (fixture.issues) {
    useAuthStore
      .getState()
      .auth.setUser({ id: 1, username: 'root', role: ROLE.SUPER_ADMIN })
  }
  const modelRatio = JSON.stringify({
    'gpt-4.1-mini': 0.2,
    'claude-sonnet': 1.5,
  })
  const billingMode = fixture.billingMode ?? '{}'
  const billingExpr = fixture.billingExpr ?? '{}'
  const pluginBillingExpr = fixture.pluginBillingExpr ?? '{}'
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  client = queryClient
  const view = render(
    <QueryClientProvider client={client}>
      <ModelRatioVisualEditor
        savedModelPrice='{}'
        savedModelRatio={modelRatio}
        savedCacheRatio='{}'
        savedCreateCacheRatio='{}'
        savedCompletionRatio='{}'
        savedImageRatio='{}'
        savedAudioRatio='{}'
        savedAudioCompletionRatio='{}'
        savedBillingMode={billingMode}
        savedBillingExpr={billingExpr}
        savedPluginBillingExpr={pluginBillingExpr}
        modelPrice='{}'
        modelRatio={modelRatio}
        cacheRatio='{}'
        createCacheRatio='{}'
        completionRatio='{}'
        imageRatio='{}'
        audioRatio='{}'
        audioCompletionRatio='{}'
        billingMode={billingMode}
        billingExpr={billingExpr}
        pluginBillingExpr={pluginBillingExpr}
        filterMode={fixture.filterMode}
        candidateModelNames={fixture.candidateModelNames}
        onChange={vi.fn()}
        onSave={vi.fn()}
        isSaving={false}
      />
    </QueryClientProvider>
  )
  // The real page has pricing metadata loaded long before the user drags over
  // a model name, so settle the pricing query before interacting.
  await waitFor(() => expect(queryClient.isFetching()).toBe(0))
  return view
}

it('excludes plugin-priced and explicitly free models from unset prices while keeping empty and missing prices', async () => {
  await renderEditor({
    filterMode: 'unset',
    candidateModelNames: [
      'seedance-2.0-fast-official',
      'free-video',
      'empty-video',
      'missing-video',
    ],
    pluginBillingExpr: JSON.stringify({
      'async-compatible::seedance-2.0-fast-official':
        'tier("base", u("seconds") * 0.4)',
      'async-compatible::free-video': 'tier("base", 0)',
      'async-compatible::empty-video': ' ',
    }),
  })
  expect(
    screen.queryByRole('row', { name: /seedance-2\.0-fast-official/ })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('row', { name: /free-video/ })
  ).not.toBeInTheDocument()
  expect(screen.getByRole('row', { name: /empty-video/ })).toBeVisible()
  expect(screen.getByRole('row', { name: /missing-video/ })).toBeVisible()
})

it('shows a plugin-only price as an expression without changing its saved ordinary mode', async () => {
  await renderEditor({
    taskModels: ['seedance-2.0-fast-official'],
    pluginBillingExpr: JSON.stringify({
      'async-compatible::seedance-2.0-fast-official':
        'tier("base", u("seconds") * 0.4)',
    }),
  })
  const row = screen.getByRole('row', {
    name: /seedance-2\.0-fast-official/,
  })
  expect(within(row).getByText('Expression')).toBeVisible()
  expect(
    within(row).queryByText('Per-token (deprecated)')
  ).not.toBeInTheDocument()
  expect(within(row).queryByText('Unset price')).not.toBeInTheDocument()
  const user = userEvent.setup()
  const filter = screen
    .getAllByRole('button', { name: 'Mode' })
    .find((button) => button.getAttribute('aria-haspopup') === 'dialog')
  if (!filter) throw new Error('Mode filter not found')
  await user.click(filter)
  await user.click(
    await screen.findByRole('option', { name: /Expression - Task pricing/ })
  )
  expect(row).toBeInTheDocument()
  expect(
    screen.queryByRole('row', { name: /gpt-4\.1-mini/ })
  ).not.toBeInTheDocument()
})

// A drag-select ends with mousedown/mouseup inside the row and the browser
// then fires `click` on the row. Only that final `click` is dispatched here so
// the text selection created by the drag is still active when the row opens
// the editor.
it('keeps the drag-selected model name highlighted when the row click opens the editor', async () => {
  await renderEditor()
  const row = await screen.findByRole('row', { name: /gpt-4\.1-mini/ })
  const nameCell = within(row).getByText('gpt-4.1-mini')
  window.getSelection()?.selectAllChildren(nameCell)
  expect(String(window.getSelection())).toBe('gpt-4.1-mini')

  fireEvent.click(nameCell)

  expect(
    await screen.findByRole('region', { name: 'Edit model pricing' })
  ).toBeInTheDocument()
  expect(nameCell).toBeInTheDocument()
  expect(String(window.getSelection())).toBe('gpt-4.1-mini')
})

it('keeps other rows mounted when a different model is opened for editing', async () => {
  await renderEditor()
  const targetRow = await screen.findByRole('row', { name: /gpt-4\.1-mini/ })
  const otherName = within(
    screen.getByRole('row', { name: /claude-sonnet/ })
  ).getByText('claude-sonnet')

  fireEvent.click(within(targetRow).getByText('gpt-4.1-mini'))

  expect(
    await screen.findByRole('region', { name: 'Edit model pricing' })
  ).toBeInTheDocument()
  expect(otherName).toBeInTheDocument()
})

// kling-v3 was never priced, issue-shared's model expression reads usage its
// new provider lacks, and vidu1.5 keeps a provider price its plugin dropped.
const pricingIssueFixture: EditorFixture = {
  taskModels: ['issue-shared'],
  billingMode: JSON.stringify({ 'issue-shared': 'tiered_expr' }),
  billingExpr: JSON.stringify({ 'issue-shared': 'u("seconds") * 0.2' }),
  pluginBillingExpr: JSON.stringify({ 'vidu::vidu1.5': 'u("seconds") * 0.1' }),
  issues: [
    {
      model_name: 'issue-shared',
      plugin_key: 'beta',
      plugin_name: 'Beta',
      reason: 'incompatible',
    },
    {
      model_name: 'kling-v3',
      plugin_key: 'kling',
      plugin_name: 'Kling',
      reason: 'unconfigured',
    },
    {
      model_name: 'vidu1.5',
      plugin_key: 'vidu',
      plugin_name: 'Vidu',
      reason: 'stale',
    },
  ],
}

it.each([
  ['issue-shared', 'Beta: The price uses usage the plugin no longer provides'],
  ['kling-v3', 'Kling: No price set'],
  ['vidu1.5', 'Vidu: The plugin no longer offers this model'],
])(
  'marks %s as needing price attention with the reason instead of a configured price',
  async (name, reason) => {
    await renderEditor(pricingIssueFixture)
    const row = await screen.findByRole('row', {
      name: new RegExp(name.replace('.', '\\.')),
    })

    expect(within(row).getByText('Price needs attention')).toBeVisible()
    expect(within(row).getByText(reason)).toBeVisible()
    expect(within(row).queryByText('Task pricing')).not.toBeInTheDocument()
  }
)

it('keeps the configured badges on models without pricing issues', async () => {
  await renderEditor({ ...pricingIssueFixture, issues: [] })
  const row = await screen.findByRole('row', { name: /issue-shared/ })

  expect(within(row).getByText('Task pricing')).toBeVisible()
  expect(
    within(row).queryByText('Price needs attention')
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('row', { name: /kling-v3/ })
  ).not.toBeInTheDocument()
})

it('filters the table down to the models whose price needs attention', async () => {
  const user = userEvent.setup()
  await renderEditor(pricingIssueFixture)
  await screen.findByRole('row', { name: /kling-v3/ })
  const filter = screen.getByRole('button', { name: /Price status/ })

  await user.click(filter)
  await user.click(
    await screen.findByRole('option', { name: /Price needs attention/ })
  )

  await waitFor(() =>
    expect(
      screen.queryByRole('row', { name: /gpt-4\.1-mini/ })
    ).not.toBeInTheDocument()
  )
  for (const name of [/issue-shared/, /kling-v3/, /vidu1\.5/]) {
    expect(screen.getByRole('row', { name })).toBeInTheDocument()
  }
  expect(
    screen.queryByRole('row', { name: /claude-sonnet/ })
  ).not.toBeInTheDocument()
  expect(within(filter).getByText('Price needs attention')).toBeInTheDocument()
})
