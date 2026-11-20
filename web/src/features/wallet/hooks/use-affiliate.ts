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
import i18next from 'i18next'
import { useState, useCallback } from 'react'
import { toast } from 'sonner'

import { useCopyToClipboard } from '@/hooks/use-copy-to-clipboard'
import { getSelf } from '@/lib/api'
import { handleServerError } from '@/lib/handle-server-error'
import {
  getServerMessage,
  requireServerSuccess,
} from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import {
  getAffiliateCode,
  getReferralOverview,
  getUnifiedWallet,
  transferAffiliateQuota,
} from '../api'
import { generateAffiliateLink } from '../lib'

// ============================================================================
// Affiliate Hook
// ============================================================================

export function useAffiliate() {
  const userID = useAuthStore((state) => state.auth.user?.id)
  const sessionID = useAuthStore((state) => state.auth.session?.sid)
  const walletQuery = useQuery({
    queryKey: ['unified-wallet', userID, sessionID],
    queryFn: ({ signal }) => getUnifiedWallet(signal),
    enabled: Boolean(userID),
    retry: false,
    refetchInterval: 15_000,
    meta: { errorToast: false },
  })
  const legacy = walletQuery.data?.enabled === false
  const query = useQuery({
    queryKey: ['referral-overview', userID, sessionID],
    queryFn: ({ signal }) => getReferralOverview(signal),
    enabled: Boolean(userID) && walletQuery.data?.enabled === true,
    retry: false,
    refetchInterval: 15_000,
    meta: { errorToast: false },
  })
  const legacyQuery = useQuery({
    queryKey: ['affiliate-code', userID, sessionID],
    queryFn: async ({ signal }) =>
      requireServerSuccess(await getAffiliateCode(signal)),
    enabled: Boolean(userID) && legacy,
    retry: false,
    meta: { errorToast: false },
  })
  const affiliateCode = legacy
    ? (legacyQuery.data?.data ?? '')
    : (query.data?.code ?? '')
  const affiliateLink = affiliateCode
    ? generateAffiliateLink(affiliateCode)
    : ''
  const [transferring, setTransferring] = useState(false)
  const { copyToClipboard } = useCopyToClipboard()

  // Copy affiliate link
  const copyAffiliateLink = useCallback(() => {
    copyToClipboard(affiliateLink)
  }, [affiliateLink, copyToClipboard])

  // Transfer affiliate quota to balance
  const transferQuota = useCallback(async (quota: number): Promise<boolean> => {
    try {
      setTransferring(true)
      const response = requireServerSuccess(
        await transferAffiliateQuota({ quota })
      )

      if (response.success) {
        toast.success(
          getServerMessage(response) || i18next.t('Transfer successful')
        )
        await getSelf()
        return true
      }

      handleServerError(response, i18next.t('Transfer failed'))
      return false
    } catch (_error) {
      handleServerError(_error, i18next.t('Transfer failed'))
      return false
    } finally {
      setTransferring(false)
    }
  }, [])

  const refetch = () => {
    if (walletQuery.isError) return walletQuery.refetch()
    if (legacy) return legacyQuery.refetch()
    return query.refetch()
  }

  return {
    affiliateCode,
    affiliateLink,
    legacy,
    overview: query.data,
    loading:
      walletQuery.isPending ||
      (!walletQuery.isError &&
        (legacy ? legacyQuery.isPending : query.isPending)),
    error:
      walletQuery.isError || (legacy ? legacyQuery.isError : query.isError),
    transferring,
    copyAffiliateLink,
    transferQuota,
    refetch,
  }
}
