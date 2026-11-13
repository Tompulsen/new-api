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
import { z } from 'zod'

export const communitySsoSearchSchema = z.object({
  client_id: z.string(),
  redirect_uri: z.string(),
  response_type: z.literal('code'),
  scope: z.enum(['identity', 'chat_session']),
  expected_subject: z.preprocess(
    // TanStack's default search parser decodes unquoted account IDs as numbers.
    // Normalize only exact positive integers; do not coerce arbitrary input.
    (value) =>
      typeof value === 'number' && Number.isSafeInteger(value) && value > 0
        ? String(value)
        : value,
    z
      .string()
      .regex(/^[1-9]\d{0,15}$/)
      .optional()
  ),
  state: z.string().regex(/^[A-Za-z0-9_-]{43}$/),
  code_challenge: z.string().regex(/^[A-Za-z0-9_-]{43}$/),
  code_challenge_method: z.literal('S256'),
})

export type CommunitySsoConfig = {
  enabled: boolean
  community_origin: string
  studio_origin: string
  community_origins?: string[]
  studio_origins?: string[]
}

export function communitySsoTargetOrigin(
  search: z.infer<typeof communitySsoSearchSchema>,
  config: CommunitySsoConfig
): string | undefined {
  if (!config.enabled) return undefined
  const studio = search.scope === 'chat_session'
  const origins = studio
    ? (config.studio_origins ?? [config.studio_origin])
    : (config.community_origins ?? [config.community_origin])
  const callback = studio
    ? '/api/new-api/community/callback'
    : '/api/newapi/callback'
  if (
    origins.includes(search.client_id) &&
    search.redirect_uri === `${search.client_id}${callback}`
  ) {
    return search.client_id
  }
  return undefined
}

export function communitySsoCallback(
  raw: string,
  search: z.infer<typeof communitySsoSearchSchema>
): string {
  const target = new URL(raw)
  if (
    target.origin + target.pathname !== search.redirect_uri ||
    target.username ||
    target.password ||
    target.hash ||
    target.searchParams.getAll('state').length !== 1 ||
    target.searchParams.get('state') !== search.state ||
    target.searchParams.getAll('code').length !== 1 ||
    !/^[A-Za-z0-9_-]{43}$/.test(target.searchParams.get('code') ?? '')
  )
    throw new Error('Invalid callback')
  return target.href
}
