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
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import { formatTimestampToDate } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import { getUnifiedWallet } from '../api'
import { formatCommunityPoints } from '../lib/format'

export function UnifiedWalletCard() {
  const { t, i18n } = useTranslation()
  const userID = useAuthStore((state) => state.auth.user?.id)
  const sessionID = useAuthStore((state) => state.auth.session?.sid)
  const query = useQuery({
    queryKey: ['unified-wallet', userID, sessionID],
    queryFn: ({ signal }) => getUnifiedWallet(signal),
    enabled: Boolean(userID),
    retry: false,
    meta: { errorToast: false },
  })
  if (!userID) return null
  if (query.isPending) {
    return <LoadingState inline message={t('Loading community points')} />
  }
  if (query.isError) {
    return (
      <ErrorState
        title={t('Community points are unavailable')}
        onRetry={() => void query.refetch()}
        className='min-h-0 py-4'
      />
    )
  }
  if (!query.data.enabled) return null
  const language = i18n.resolvedLanguage || i18n.language
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('Community points')}</CardTitle>
        <CardDescription>{t('100 points = ¥0.02')}</CardDescription>
        <CardAction>
          <Button
            variant='outline'
            size='sm'
            aria-label={t('Refresh community points')}
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t('Refresh')}
          </Button>
        </CardAction>
      </CardHeader>
      <CardContent>
        <dl className='grid gap-4 sm:grid-cols-2'>
          <div>
            <dt className='font-medium'>{t('Charity points')}</dt>
            <dd className='mt-1 font-mono text-xl break-all tabular-nums'>
              {formatCommunityPoints(query.data.charity_points, language)}
            </dd>
            <dd className='text-muted-foreground mt-1 text-sm'>
              {t(
                'Only available in the community. Original expiry dates are preserved.'
              )}
            </dd>
            {query.data.charity_expiring_at != null && (
              <dd className='text-muted-foreground mt-1 text-sm'>
                {t('Next charity expiry: {{date}}', {
                  date: formatTimestampToDate(query.data.charity_expiring_at),
                })}
              </dd>
            )}
          </div>
          <div>
            <dt className='font-medium'>{t('Permanent points')}</dt>
            <dd className='mt-1 font-mono text-xl break-all tabular-nums'>
              {formatCommunityPoints(query.data.permanent_points, language)}
            </dd>
            <dd className='text-muted-foreground mt-1 text-sm'>
              {t('The community view of your paid New API balance.')}
            </dd>
          </div>
        </dl>
      </CardContent>
    </Card>
  )
}
