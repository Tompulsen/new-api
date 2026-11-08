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
    z.string().regex(/^[1-9]\d{0,15}$/).optional()
  ),
  state: z.string().regex(/^[A-Za-z0-9_-]{43}$/),
  code_challenge: z.string().regex(/^[A-Za-z0-9_-]{43}$/),
  code_challenge_method: z.literal('S256'),
})
