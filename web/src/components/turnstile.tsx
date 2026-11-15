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
import { useEffect, useEffectEvent, useRef } from 'react'

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
  const ref = useRef<HTMLDivElement | null>(null)
  const onVerify = useEffectEvent((token: string) => props.onVerify(token))
  const onExpire = useEffectEvent(() => {
    props.onExpire?.()
  })

  useEffect(() => {
    let active = true
    let widgetId: string | undefined
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
        widgetId = window.turnstile.render(ref.current, {
          sitekey: props.siteKey,
          action: props.action,
          'refresh-expired': 'auto',
          retry: 'auto',
          callback: (token: string) => {
            if (active) onVerify(token)
          },
          'error-callback': () => {
            if (active) onExpire()
          },
          'expired-callback': () => {
            if (active) onExpire()
          },
          'timeout-callback': () => {
            if (active) onExpire()
          },
        })
      } catch {
        onExpire()
      }
    }
    const onError = () => {
      script?.remove()
      if (active) onExpire()
    }
    const scriptId = 'cf-turnstile'
    let script = document.querySelector<HTMLScriptElement>(`#${scriptId}`)
    if (!window.turnstile) {
      if (!script) {
        script = document.createElement('script')
        script.id = scriptId
        script.src =
          'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit'
        script.async = true
        script.defer = true
        document.head.appendChild(script)
      }
      script.addEventListener('load', render)
      script.addEventListener('error', onError)
    } else {
      render()
    }
    return () => {
      active = false
      script?.removeEventListener('load', render)
      script?.removeEventListener('error', onError)
      if (widgetId !== undefined) window.turnstile?.remove(widgetId)
    }
  }, [props.siteKey, props.action])

  return <div ref={ref} className={props.className} />
}
