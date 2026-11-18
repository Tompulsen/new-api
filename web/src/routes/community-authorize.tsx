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
import { useQuery } from '@tanstack/react-query'
import {
  createFileRoute,
  redirect,
  useLocation,
  useNavigate,
} from '@tanstack/react-router'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import { useAuthRedirect } from '@/features/auth/hooks/use-auth-redirect'
import {
  communitySsoSearchSchema,
  communitySsoTargetOrigin,
  communitySsoCallback,
  type CommunitySsoConfig,
} from '@/features/auth/lib/community-sso-search'
import { getQXChatIdentity } from '@/features/auth/qxchat'
import { api, resolveAuthentication } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

export const Route = createFileRoute('/community-authorize')({
  validateSearch: communitySsoSearchSchema,
  beforeLoad: async ({ location }) => {
    await resolveAuthentication()
    if (!useAuthStore.getState().auth.accessToken) {
      throw redirect({ to: '/sign-in', search: { redirect: location.href } })
    }
  },
  loader: async () => {
    const { data } = await api.get('/api/community-sso/config', {
      skipErrorHandler: true,
    })
    return data as CommunitySsoConfig
  },
  component: CommunityAuthorize,
})

function CommunityAuthorize() {
  const search = Route.useSearch()
  const config = Route.useLoaderData()
  const user = useAuthStore((state) => state.auth.user)
  const { t } = useTranslation()
  const navigate = useNavigate()
  const destination = useLocation({ select: (location) => location.href })
  const { redirectToLogin } = useAuthRedirect()
  const [busy, setBusy] = useState(false)
  const [failed, setFailed] = useState(false)
  const studio = search.scope === 'chat_session'
  const targetOrigin = communitySsoTargetOrigin(search, config)
  const qxchat =
    !studio &&
    Boolean(targetOrigin && config.qxchat_origins?.includes(targetOrigin))
  const identity = useQuery({
    queryKey: ['qxchat', 'identity', user?.id],
    queryFn: ({ signal }) => getQXChatIdentity(signal),
    enabled: qxchat && Boolean(user?.id),
    staleTime: 0,
    refetchOnMount: 'always',
    retry: false,
    meta: { errorToast: false },
  })
  const profileReady =
    identity.isSuccess &&
    !identity.isFetching &&
    identity.data.enabled &&
    (identity.data.profile_kind === 'original' ||
      identity.data.profile_kind === 'independent')
  const independent =
    qxchat && profileReady && identity.data.profile_kind === 'independent'
  const accountMatches = !studio || String(user?.id) === search.expected_subject
  const valid = Boolean(targetOrigin)
  const canContinue = valid && accountMatches && (!qxchat || profileReady)
  let title = t('Continue to the community')
  let hint = t(
    'Your first sign-in creates a community profile. Future sign-ins use the same profile. Your 章鱼 balance and API keys stay with this account.'
  )
  if (studio) {
    title = t('Continue to the creation studio')
    hint = t(
      'The studio will use this account. Model usage is charged to this 章鱼 account through your own API key.'
    )
  } else if (qxchat) {
    title = t('Choose your QXChat profile')
    hint = t(
      'Original QXChat points and records belong to their original profile. Accounts are not merged by a matching email or display name.'
    )
  }
  let continueText = t('Continue with this account')
  if (independent) continueText = t('Continue with this separate profile')
  if (busy) continueText = t('Continuing…')
  async function authorize() {
    if (!canContinue || busy) return
    setBusy(true)
    setFailed(false)
    try {
      const { data } = await api.post(
        studio ? '/api/studio-sso/authorize' : '/api/community-sso/authorize',
        search,
        { skipErrorHandler: true }
      )
      window.location.assign(communitySsoCallback(data.redirect_uri, search))
    } catch {
      setFailed(true)
      setBusy(false)
    }
  }
  return (
    <main className='bg-background text-foreground flex min-h-svh items-center justify-center px-5 py-12'>
      <section
        className='w-full max-w-md space-y-6'
        aria-busy={busy || (qxchat && identity.isFetching)}
      >
        <h1 className='text-2xl font-semibold'>{title}</h1>
        <div className='space-y-1 text-base leading-7 break-words'>
          <p>{t('Current account:')}</p>
          <p className='font-semibold'>
            {user?.username} · #{user?.id}
          </p>
          {user?.display_name && user.display_name !== user.username && (
            <p className='text-muted-foreground text-sm'>{user.display_name}</p>
          )}
        </div>
        <p className='text-muted-foreground text-sm leading-6'>{hint}</p>
        {qxchat && identity.isFetching && (
          <LoadingState inline message={t('Checking your QXChat profile…')} />
        )}
        {qxchat && !identity.isFetching && !profileReady && (
          <ErrorState
            className='min-h-0 py-3'
            title={t('QXChat profile unavailable')}
            description={t(
              'Your account could not be checked. Retry before continuing.'
            )}
            onRetry={() => void identity.refetch()}
          />
        )}
        {qxchat &&
          profileReady &&
          identity.data.profile_kind === 'original' && (
            <p className='rounded-lg border p-4 text-sm leading-6 break-words'>
              {t('Original QXChat profile: {{username}}', {
                username: identity.data.username,
              })}
            </p>
          )}
        {independent && (
          <p className='rounded-lg border p-4 text-sm leading-6'>
            {t(
              'This New API account uses a separate QXChat profile. On first entry it starts with 0 points; original QXChat points and records stay in the original account. To use them, choose your original QXChat account below.'
            )}
          </p>
        )}
        {valid ? (
          <p className='text-sm break-all'>{targetOrigin}</p>
        ) : (
          <p role='alert'>
            {t(
              'This sign-in request is invalid. Return to the originating site and try again.'
            )}
          </p>
        )}
        {!accountMatches && (
          <p role='alert'>
            {t(
              'This is not the 章鱼 account linked to your community profile. Switch accounts in the account center before continuing.'
            )}
          </p>
        )}
        {failed && (
          <p role='alert'>{t('Could not continue. Please try again.')}</p>
        )}
        <div className='flex flex-col gap-3'>
          {qxchat && (
            <Button
              className='h-auto min-h-11 py-2 whitespace-normal'
              variant={independent ? 'default' : 'outline'}
              disabled={busy || !valid}
              onClick={() => redirectToLogin(destination, true)}
            >
              {t('Use my original QXChat account')}
            </Button>
          )}
          <Button
            className='h-auto min-h-11 py-2 whitespace-normal'
            variant={independent ? 'outline' : 'default'}
            disabled={!canContinue || busy}
            onClick={() => void authorize()}
          >
            {continueText}
          </Button>
          <Button
            className='min-h-11'
            variant='ghost'
            disabled={busy}
            onClick={() => void navigate({ to: '/dashboard' })}
          >
            {t('Cancel')}
          </Button>
        </div>
      </section>
    </main>
  )
}
