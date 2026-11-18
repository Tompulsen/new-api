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
import { KeyRound } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { TitledCard } from '@/components/ui/titled-card'
import { api } from '@/lib/api'

interface LoginIdentifiers {
  username: string
  email: string
  login_identifiers: string[]
}

export function LoginIdentifiersCard({ userId }: { userId: number }) {
  const { t } = useTranslation()
  const query = useQuery({
    queryKey: ['login-identifiers', userId],
    queryFn: async ({ signal }) => {
      const { data: response } = await api.get<{
        success: boolean
        data?: LoginIdentifiers
      }>('/api/user/self/login-identifiers', { signal })
      const data = response.data
      if (
        !response.success ||
        !data ||
        typeof data.username !== 'string' ||
        typeof data.email !== 'string' ||
        !Array.isArray(data.login_identifiers) ||
        data.login_identifiers.some((value) => typeof value !== 'string')
      ) {
        throw new Error(t('Unable to load login names. Please try again.'))
      }
      return data
    },
    retry: false,
    meta: { errorToast: false },
  })
  return (
    <TitledCard
      title={t('Login names')}
      icon={<KeyRound className='size-4' />}
      disableHoverEffect
    >
      {query.isPending && <p role='status'>{t('Loading...')}</p>}
      {query.isError && (
        <div className='space-y-2'>
          <p role='alert'>
            {t('Unable to load login names. Please try again.')}
          </p>
          <Button
            type='button'
            variant='outline'
            onClick={() => void query.refetch()}
          >
            {t('Retry')}
          </Button>
        </div>
      )}
      {query.isSuccess && (
        <dl className='grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-3 text-sm'>
          <dt className='text-muted-foreground'>{t('Unified account')}</dt>
          <dd className='break-all'>{query.data.username}</dd>
          <dt className='text-muted-foreground'>{t('Email')}</dt>
          <dd className='break-all'>{query.data.email || t('Not set')}</dd>
          <dt className='text-muted-foreground'>
            {t('Available login names')}
          </dt>
          <dd className='space-y-1 break-all'>
            {query.data.login_identifiers.map((value) => (
              <p key={value}>{value}</p>
            ))}
            {!query.data.login_identifiers.length &&
              t(
                'No unique login name is available. Please update your account.'
              )}
          </dd>
        </dl>
      )}
    </TitledCard>
  )
}
