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
import { useSearch } from '@tanstack/react-router'
import { useCallback, useEffect, useRef } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { handleServerError } from '@/lib/handle-server-error'
import { AuthOperationError } from '@/lib/secure-verification'
import { useAuthStore } from '@/stores/auth-store'

import { useAuthRedirect } from '../../hooks/use-auth-redirect'
import {
  SecureVerificationDialog,
  useSecureVerification,
} from '../../secure-verification'

export function OtpForm() {
  const { t } = useTranslation()
  const search = useSearch({ strict: false })
  // Transfer the pending challenge from the navigation handoff into this page's
  // lifetime. Reloading or leaving the page cannot resume it from browser storage.
  const pending = useRef(
    useAuthStore.getState().auth.pendingLoginVerification
  ).current
  const initialSessionID = useRef(
    useAuthStore.getState().auth.session?.sid
  ).current
  const sessionID = useAuthStore((state) => state.auth.session?.sid)
  const verification = useSecureVerification()
  const { requestLoginVerification, cancel } = verification
  const { handleLoginSuccess, redirectToLogin } = useAuthRedirect()
  const completed = useRef(false)
  const retryRedirect = pending?.redirectTo ?? search.redirect
  const retryQXChat = pending?.qxchatEntry ?? search.qxchat === '1'
  const retryLogin = useCallback(
    () => redirectToLogin(retryRedirect, retryQXChat),
    [redirectToLogin, retryRedirect, retryQXChat]
  )

  useEffect(() => {
    if (completed.current) return
    if (!pending) {
      retryLogin()
      return
    }
    if (sessionID !== initialSessionID) {
      cancel()
      return
    }
    if (useAuthStore.getState().auth.pendingLoginVerification === pending) {
      useAuthStore.getState().auth.setPendingLoginVerification(null)
    }
    let active = true
    void (async () => {
      try {
        const bundle = await requestLoginVerification(pending.challenge)
        if (
          !active ||
          useAuthStore.getState().auth.session?.sid !== initialSessionID
        ) {
          return
        }
        if (!bundle) {
          retryLogin()
          return
        }
        completed.current = true
        await handleLoginSuccess(bundle, pending.redirectTo)
        toast.success(t('Signed in'))
      } catch (error) {
        if (!active) return
        handleServerError(AuthOperationError.from(error))
        retryLogin()
      }
    })()
    return () => {
      active = false
      cancel()
    }
  }, [
    pending,
    initialSessionID,
    sessionID,
    requestLoginVerification,
    cancel,
    handleLoginSuccess,
    retryLogin,
    t,
  ])

  return <SecureVerificationDialog {...verification.dialogProps} />
}
