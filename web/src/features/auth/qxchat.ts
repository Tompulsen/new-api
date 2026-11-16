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
import type { AccountSecurityResult } from '@/features/profile/types'
import { api } from '@/lib/api'
import { authRequestOptions, authResult } from '@/lib/secure-verification'

import {
  clearPasswordEncryptionCache,
  encryptPassword,
} from './lib/password-encryption'

export interface QXChatIdentityStatus {
  enabled: boolean
  state: 'unbound' | 'pending' | 'active'
  username?: string
}

export function getQXChatConfig(
  signal?: AbortSignal
): Promise<{ enabled: boolean }> {
  return authResult(
    api.get('/api/user/qxchat/config', {
      ...authRequestOptions,
      skipAuthRefresh: true,
      signal,
    })
  )
}

export function getQXChatIdentity(
  signal?: AbortSignal
): Promise<QXChatIdentityStatus> {
  return authResult(
    api.get('/api/user/qxchat', { ...authRequestOptions, signal })
  )
}

export async function bindQXChatIdentity(
  username: string,
  password: string,
  passwordEncryptionEnabled: boolean,
  proofToken: string,
  signal: AbortSignal
): Promise<AccountSecurityResult> {
  try {
    const passwordFields = passwordEncryptionEnabled
      ? await encryptPassword(password)
      : { password }
    signal.throwIfAborted()
    return await authResult(
      api.post(
        '/api/user/qxchat/bind',
        { username, ...passwordFields },
        {
          ...authRequestOptions,
          headers: { 'X-Security-Proof': proofToken },
          singleUseAuthorization: true,
          signal,
        }
      )
    )
  } catch (error) {
    if (passwordEncryptionEnabled) clearPasswordEncryptionCache()
    throw error
  }
}
