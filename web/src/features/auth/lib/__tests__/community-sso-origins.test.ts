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
import { describe, expect, test } from 'vitest'

import {
  communitySsoSearchSchema,
  communitySsoTargetOrigin,
  communitySsoCallback,
  type CommunitySsoConfig,
} from '../community-sso-search'

const primary = 'https://www.zhangyuapi.com'
const added = 'https://zaiduyu.top'
const config: CommunitySsoConfig = {
  enabled: true,
  community_origin: primary,
  studio_origin: primary,
  community_origins: [primary, added],
  studio_origins: [primary, added],
}
function request(
  origin: string,
  scope: 'identity' | 'chat_session' = 'identity'
) {
  return communitySsoSearchSchema.parse({
    client_id: origin,
    redirect_uri:
      origin +
      (scope === 'identity'
        ? '/api/newapi/callback'
        : '/api/new-api/community/callback'),
    response_type: 'code',
    scope,
    state: 's'.repeat(43),
    code_challenge: 'c'.repeat(43),
    code_challenge_method: 'S256',
  })
}
describe('explicit community and studio authorization targets', () => {
  test.each([primary, added])(
    'accepts configured origin %s in both flows',
    (origin) => {
      for (const scope of ['identity', 'chat_session'] as const) {
        expect(communitySsoTargetOrigin(request(origin, scope), config)).toBe(
          origin
        )
      }
    }
  )
  test('preserves the scalar config and rejects a newly added origin without its list', () => {
    const legacy = {
      enabled: true,
      community_origin: primary,
      studio_origin: primary,
    }
    expect(communitySsoTargetOrigin(request(primary), legacy)).toBe(primary)
    expect(communitySsoTargetOrigin(request(added), legacy)).toBeUndefined()
  })
  test('rejects suffix hosts, mismatched callbacks and disabled configuration', () => {
    expect(
      communitySsoTargetOrigin(request(`${added}.evil.example`), config)
    ).toBeUndefined()
    expect(
      communitySsoTargetOrigin(
        { ...request(added), redirect_uri: request(primary).redirect_uri },
        config
      )
    ).toBeUndefined()
    expect(
      communitySsoTargetOrigin(request(added), { ...config, enabled: false })
    ).toBeUndefined()
    expect(
      communitySsoTargetOrigin(request(added, 'chat_session'), {
        ...config,
        studio_origins: [primary],
      })
    ).toBeUndefined()
  })
  test('only returns a callback with the requested origin, path, code and state', () => {
    const search = request(added)
    const callback = `${search.redirect_uri}?code=${'c'.repeat(43)}&state=${search.state}`
    expect(communitySsoCallback(callback, search)).toBe(callback)
    for (const invalid of [
      callback.replace(added, primary),
      callback.replace('/callback', '/other'),
      callback.replace(search.state, 'x'.repeat(43)),
      `${callback}&state=${search.state}`,
      `${callback}&code=${'d'.repeat(43)}`,
      `${callback}#fragment`,
      callback.replace('c'.repeat(43), 'short'),
      callback.replace('https://', 'https://user@'),
    ]) {
      expect(() => communitySsoCallback(invalid, search)).toThrow(
        'Invalid callback'
      )
    }
  })
})
