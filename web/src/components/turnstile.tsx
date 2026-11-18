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
import { useEffect, useEffectEvent, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'

declare global {
  interface Window {
    turnstile?: {
      render: (element: HTMLElement, options: Record<string, unknown>) => string
      remove: (widgetId: string) => void
    }
  }
}

interface TurnstileProps {
  siteKey: string
  action?: string
  onVerify: (token: string) => void
  onExpire?: () => void
  className?: string
}

export function Turnstile(props: TurnstileProps) {
  const { t } = useTranslation()
  const ref = useRef<HTMLDivElement | null>(null)
  const [attempt, setAttempt] = useState(0)
  const [status, setStatus] = useState<'loading' | 'ready' | 'error'>('loading')
  const onVerify = useEffectEvent((token: string) => props.onVerify(token))
  const onExpire = useEffectEvent(() => {
    props.onExpire?.()
  })

  useEffect(() => {
    let active = true
    let verified = false
    let widgetId: string | undefined
    let script = document.querySelector<HTMLScriptElement>('#cf-turnstile')
    let loadingTimeout: number | undefined
    const fail = () => {
      if (!active) return
      verified = false
      window.clearTimeout(loadingTimeout)
      setStatus('error')
      onExpire()
    }
    const waitForVerification = () => {
      if (!active || verified) return
      setStatus('loading')
      window.clearTimeout(loadingTimeout)
      loadingTimeout = window.setTimeout(() => {
        if (!window.turnstile) script?.remove()
        fail()
      }, 15000)
    }
    const render = () => {
      if (
        !active ||
        widgetId !== undefined ||
        !ref.current ||
        !window.turnstile
      ) {
        return
      }
      try {
        // Creating a widget does not mean its iframe loaded or verification finished.
        waitForVerification()
        widgetId = window.turnstile.render(ref.current, {
          sitekey: props.siteKey,
          action: props.action,
          'refresh-expired': 'auto',
          retry: 'auto',
          callback: (token: string) => {
            if (active) {
              if (!token) {
                fail()
                return
              }
              verified = true
              window.clearTimeout(loadingTimeout)
              setStatus('ready')
              onVerify(token)
            }
          },
          'error-callback': fail,
          'unsupported-callback': fail,
          'before-interactive-callback': () => {
            if (!active) return
            window.clearTimeout(loadingTimeout)
            setStatus('ready')
          },
          'after-interactive-callback': waitForVerification,
          'expired-callback': () => {
            if (active) {
              verified = false
              onExpire()
              waitForVerification()
            }
          },
          'timeout-callback': fail,
        })
      } catch {
        fail()
      }
    }
    const onError = () => {
      script?.remove()
      fail()
    }
    const scriptId = 'cf-turnstile'
    waitForVerification()
    if (!window.turnstile) {
      if (!script) {
        script = document.createElement('script')
        script.id = scriptId
        script.src =
          'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit'
        script.async = true
        script.defer = true
      }
      script.addEventListener('load', render)
      script.addEventListener('error', onError)
      if (!script.isConnected) document.head.appendChild(script)
    } else {
      render()
    }
    return () => {
      active = false
      window.clearTimeout(loadingTimeout)
      script?.removeEventListener('load', render)
      script?.removeEventListener('error', onError)
      if (widgetId !== undefined) window.turnstile?.remove(widgetId)
    }
  }, [props.siteKey, props.action, attempt])

  return (
    <div className={props.className}>
      <div ref={ref} />
      {status === 'loading' && (
        <div role='status'>
          <LoadingState
            inline
            size='sm'
            message={t('Please wait a moment, human check is initializing...')}
          />
        </div>
      )}
      {status === 'error' && (
        <div role='alert'>
          <ErrorState
            className='min-h-0 gap-3 p-3'
            title={t('Verification failed')}
            description={t(
              'Security verification could not finish. Retry, update your app, or open this site in your phone browser.'
            )}
            onRetry={() => setAttempt((current) => current + 1)}
          />
        </div>
      )}
    </div>
  )
}
