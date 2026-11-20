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
import { Share2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { ErrorState } from '@/components/error-state'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { IconBadge } from '@/components/ui/icon-badge'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber, formatQuota } from '@/lib/format'

import type { ReferralOverview, UserWalletData } from '../types'

interface AffiliateRewardsCardProps {
  user: UserWalletData | null
  affiliateLink: string
  onTransfer: () => void
  legacy?: boolean
  overview?: ReferralOverview
  error?: boolean
  onRetry: () => void
  complianceConfirmed?: boolean
  loading?: boolean
}

export function AffiliateRewardsCard({
  user,
  affiliateLink,
  onTransfer,
  legacy = false,
  overview,
  error,
  onRetry,
  complianceConfirmed = true,
  loading,
}: AffiliateRewardsCardProps) {
  const { t, i18n } = useTranslation()
  if (loading) {
    return (
      <Card data-card-hover='false' className='bg-muted/20 py-0'>
        <CardContent className='grid gap-4 p-3 sm:p-4 lg:grid-cols-[minmax(220px,1fr)_minmax(220px,0.72fr)_minmax(320px,1.15fr)] lg:items-center'>
          <div>
            <Skeleton className='h-5 w-32' />
            <Skeleton className='mt-2 h-4 w-48' />
          </div>
          <Skeleton className='h-14 rounded-lg' />
          <Skeleton className='h-10 rounded-lg' />
        </CardContent>
      </Card>
    )
  }
  if (error || (!legacy && !overview)) {
    return (
      <ErrorState
        title={t('Referral rewards are unavailable')}
        onRetry={onRetry}
        className='min-h-0 py-4'
      />
    )
  }

  const hasRewards = (user?.aff_quota ?? 0) > 0
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  let statistics: string[][] = []
  if (legacy) {
    statistics = [
      [t('Pending'), formatQuota(user?.aff_quota ?? 0)],
      [t('Total Earned'), formatQuota(user?.aff_history_quota ?? 0)],
      [t('Invites'), formatNumber(user?.aff_count ?? 0, locale)],
    ]
  } else if (overview) {
    statistics = [
      [t('Invites'), formatNumber(overview.invited, locale)],
      [t('Awaiting qualifying top-up'), formatNumber(overview.waiting, locale)],
      [t('Credited'), formatNumber(overview.credited, locale)],
      [t('Total Earned'), formatQuota(overview.earnedQuota)],
    ]
  }

  return (
    <Card data-card-hover='false' className='bg-muted/20 py-0'>
      <CardContent className='grid gap-3 p-3 sm:gap-4 sm:p-4 lg:grid-cols-[minmax(200px,1fr)_minmax(180px,0.65fr)_minmax(280px,1fr)] lg:items-center'>
        <div className='flex min-w-0 items-start gap-2.5'>
          <IconBadge tone='chart-3'>
            <Share2 />
          </IconBadge>
          <div className='min-w-0'>
            <h3 className='truncate text-sm font-semibold'>
              {t('Referral Program')}
            </h3>
            <p className='text-muted-foreground mt-1 text-xs'>
              {legacy
                ? t(
                    'Earn rewards when users join through your referral link. Transfer accumulated rewards to your balance anytime.'
                  )
                : t(
                    'Rewards are credited to both balances after a qualifying top-up.'
                  )}
            </p>
            {!legacy && overview && (
              <>
                <p className='text-muted-foreground mt-1 text-xs'>
                  {overview.config.minTopupCny > 0
                    ? t('Minimum qualifying top-up: {{amount}} CNY', {
                        amount: formatNumber(
                          overview.config.minTopupCny,
                          locale
                        ),
                      })
                    : t('Any positive verified payment qualifies.')}
                </p>
                {!overview.config.enabled && (
                  <p className='text-muted-foreground mt-1 text-xs'>
                    {t('New referral rewards are paused.')}
                  </p>
                )}
              </>
            )}
          </div>
        </div>

        <div
          className={
            legacy
              ? 'grid grid-cols-3 gap-2 text-center'
              : 'grid grid-cols-2 gap-2 text-center sm:grid-cols-4 lg:grid-cols-2'
          }
        >
          {statistics.map(([label, value]) => (
            <div key={label}>
              <div className='text-muted-foreground text-[10px] font-medium'>
                {label}
              </div>
              <div className='mt-0.5 truncate text-sm font-semibold tabular-nums'>
                {value}
              </div>
            </div>
          ))}
        </div>

        <div className='flex min-w-0 flex-wrap items-center gap-2'>
          <Input
            value={affiliateLink}
            readOnly
            aria-label={t('Referral link')}
            className='border-muted bg-background/70 h-9 min-w-0 flex-1 font-mono text-xs'
          />
          <CopyButton
            value={affiliateLink}
            variant='outline'
            className='bg-background size-9 shrink-0'
            iconClassName='size-4'
            tooltip={t('Copy referral link')}
            aria-label={t('Copy referral link')}
          />
          {hasRewards && (
            <Button
              onClick={onTransfer}
              disabled={!complianceConfirmed}
              className='h-auto min-h-9 basis-full px-3 whitespace-normal'
              size='sm'
            >
              {legacy
                ? t('Transfer to Balance')
                : t('Transfer previous rewards')}
            </Button>
          )}
        </div>
        {!complianceConfirmed ? (
          <p className='text-muted-foreground text-xs lg:col-span-3'>
            {t(
              'Referral reward transfer is disabled until the administrator confirms compliance terms.'
            )}
          </p>
        ) : null}
      </CardContent>
    </Card>
  )
}
