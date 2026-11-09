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
import { t } from 'i18next'

import type {
  ModelPricingChange,
  ModelPricingConfig,
} from '@/features/model-pricing/api'
import type { BillingUsageSchema } from '@/features/pricing/types'
import { api } from '@/lib/api'
import { createServerError } from '@/lib/server-error-message'

export type VideoBillingBindings = {
  channel_ids: number[]
  models: string[]
}

export type VideoBillingConfig = {
  bindings: VideoBillingBindings
  version: string
  channels: { id: number; name: string; models: string[] }[]
  models: string[]
  usage_schema: BillingUsageSchema
}

export async function getVideoBilling(): Promise<VideoBillingConfig> {
  const response = await api.get('/api/option/video_billing')
  if (!response.data.success) {
    throw createServerError(response.data, t('Failed to load model pricing'))
  }
  return response.data.data
}

export async function saveVideoBilling(request: {
  bindings: VideoBillingBindings
  expected_version: string
}): Promise<VideoBillingConfig> {
  const response = await api.put('/api/option/video_billing', request)
  if (!response.data.success) {
    throw createServerError(response.data, t('Failed to save model pricing'))
  }
  return response.data.data
}

export function videoPricingChanges(
  snapshot: ModelPricingConfig,
  names: string[],
  expression: string,
  overwrite: boolean
): { changes: ModelPricingChange[]; skipped: string[] } {
  if (!expression.trim()) throw new Error(t('Billing expression is required'))
  const entries = new Map(
    snapshot.entries.map((entry) => [entry.model_name, entry])
  )
  const changes: ModelPricingChange[] = []
  const skipped: string[] = []
  for (const name of new Set(names)) {
    const entry = entries.get(name)
    if (!entry) {
      throw new Error(t('Reload model prices before applying a template'))
    }
    if (!overwrite && Object.keys(entry.configured).length > 0) {
      skipped.push(name)
      continue
    }
    changes.push({
      model_name: name,
      expected_version: entry.version,
      pricing: {
        ...entry.configured,
        'billing_setting.plugin_billing_expr': {
          ...entry.configured['billing_setting.plugin_billing_expr'],
          sora: expression,
        },
      },
    })
  }
  return { changes, skipped }
}
