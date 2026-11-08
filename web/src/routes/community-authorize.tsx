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
import { createFileRoute, redirect } from '@tanstack/react-router'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { communitySsoSearchSchema } from '@/features/auth/lib/community-sso-search'
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
    return data as {
      enabled: boolean
      community_origin: string
      studio_origin: string
    }
  },
  component: CommunityAuthorize,
})

function CommunityAuthorize() {
  const search = Route.useSearch()
  const config = Route.useLoaderData()
  const user = useAuthStore((state) => state.auth.user)
  const { i18n } = useTranslation()
  const en = i18n.language.startsWith('en')
  const copy = en
    ? {
        studioTitle: 'Continue to the creation studio',
        communityTitle: 'Continue to the community',
        studioHint:
          'The studio will use this account. Model usage is charged to this 章鱼 account through your own API key.',
        communityHint:
          'Your first sign-in creates a community profile. Future sign-ins use the same profile. Your 章鱼 balance and API keys stay with this account.',
        continuing: 'Continuing…',
        continue: 'Continue with this account',
      }
    : {
        studioTitle: '使用对应账号进入制作空间',
        communityTitle: '使用章鱼账号进入社区',
        studioHint:
          '制作空间将使用当前账号。调用模型时，通过你自己的密钥使用此章鱼账号的余额。',
        communityHint:
          '首次登录会自动建立社区账号，之后登录使用同一账号。章鱼的余额和 API 密钥仍保留在当前账号中。',
        continuing: '正在进入…',
        continue: '使用此账号继续',
      }
  const [busy, setBusy] = useState(false)
  const [failed, setFailed] = useState(false)
  const studio = search.scope === 'chat_session'
  const targetOrigin = studio ? config.studio_origin : config.community_origin
  const callback = studio
    ? '/api/new-api/community/callback'
    : '/api/newapi/callback'
  const accountMatches = !studio || String(user?.id) === search.expected_subject
  const valid =
    Boolean(targetOrigin) &&
    config.enabled &&
    targetOrigin === search.client_id &&
    search.redirect_uri === `${targetOrigin}${callback}`
  async function authorize() {
    if (!valid || !accountMatches || busy) return
    setBusy(true)
    setFailed(false)
    try {
      const { data } = await api.post(
        studio ? '/api/studio-sso/authorize' : '/api/community-sso/authorize',
        search,
        { skipErrorHandler: true }
      )
      const target = new URL(data.redirect_uri)
      if (target.origin !== targetOrigin || target.pathname !== callback) {
        throw new Error('Invalid callback')
      }
      window.location.assign(target.href)
    } catch {
      setFailed(true)
      setBusy(false)
    }
  }
  return (
    <main className='bg-background text-foreground flex min-h-svh items-center justify-center px-5 py-12'>
      <section className='w-full max-w-md space-y-6' aria-busy={busy}>
        <h1 className='text-2xl font-semibold'>
          {studio ? copy.studioTitle : copy.communityTitle}
        </h1>
        <p className='text-base leading-7'>
          {en ? 'Current account: ' : '当前账号：'}
          <strong>{user?.display_name || user?.username}</strong>
        </p>
        <p className='text-muted-foreground text-sm leading-6'>
          {studio ? copy.studioHint : copy.communityHint}
        </p>
        {valid ? (
          <p className='text-sm break-all'>{targetOrigin}</p>
        ) : (
          <p role='alert'>
            {en
              ? 'This sign-in request is invalid. Return to the community and try again.'
              : '登录请求无效，请返回社区重新发起登录。'}
          </p>
        )}
        {!accountMatches && (
          <p role='alert'>
            {en
              ? 'This is not the 章鱼 account linked to your community profile. Switch accounts in the account center before continuing.'
              : '当前章鱼账号与社区对应的账号不一致。请先到账号中心切换账号，再返回继续。'}{' '}
            <a className='underline' href='/dashboard'>
              {en ? 'Account center' : '账号中心'}
            </a>
          </p>
        )}
        {failed && (
          <p role='alert'>
            {en
              ? 'Could not continue. Please try again.'
              : '暂时无法继续，请重试。'}
          </p>
        )}
        <div className='flex flex-wrap gap-3'>
          <Button
            className='min-h-11'
            disabled={!valid || !accountMatches || busy}
            onClick={() => void authorize()}
          >
            {busy ? copy.continuing : copy.continue}
          </Button>
          <Button
            className='min-h-11'
            variant='outline'
            disabled={busy}
            onClick={() => window.location.assign('/dashboard')}
          >
            {en ? 'Cancel' : '取消'}
          </Button>
        </div>
      </section>
    </main>
  )
}
