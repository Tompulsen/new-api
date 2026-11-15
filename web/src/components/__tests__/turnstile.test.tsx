import { act, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, expect, it, vi } from 'vitest'

import { Turnstile } from '../turnstile'

afterEach(() => {
  delete window.turnstile
  document.querySelector('#cf-turnstile')?.remove()
})

it('refreshing a widget ignores late results from the removed verification', () => {
  const callbacks: Record<string, unknown>[] = []
  window.turnstile = {
    render: (_element, options) => {
      callbacks.push(options)
      return String(callbacks.length)
    },
    remove: vi.fn(),
  }
  const verify = vi.fn()
  const view = render(
    <Turnstile
      key='first'
      siteKey='test-site'
      action='registration'
      onVerify={verify}
    />
  )
  const firstCallback = callbacks[0].callback as (token: string) => void
  view.rerender(
    <Turnstile
      key='next'
      siteKey='test-site'
      action='registration'
      onVerify={verify}
    />
  )
  verify.mockClear()
  act(() => firstCallback('old-token'))
  expect(verify).not.toHaveBeenCalled()
  act(() => (callbacks[1].callback as (token: string) => void)('new-token'))
  expect(verify).toHaveBeenLastCalledWith('new-token')
  view.unmount()
  expect(window.turnstile.remove).toHaveBeenCalledWith('1')
  expect(window.turnstile.remove).toHaveBeenCalledWith('2')
})

it('expiry and provider errors clear the usable token without rerendering the widget', () => {
  let options: Record<string, unknown> = {}
  const renderWidget = vi.fn(
    (_element: HTMLElement, value: Record<string, unknown>) => {
      options = value
      return 'widget'
    }
  )
  window.turnstile = { render: renderWidget, remove: vi.fn() }
  function Harness() {
    const [token, setToken] = useState('')
    return (
      <>
        <Turnstile
          siteKey='test-site'
          onVerify={(value) => setToken(value)}
          onExpire={() => setToken('')}
        />
        <output>{token || 'Waiting for verification'}</output>
      </>
    )
  }
  render(<Harness />)
  act(() => (options.callback as (value: string) => void)('first-token'))
  expect(screen.getByText('first-token')).toBeInTheDocument()
  act(() => (options['expired-callback'] as () => void)())
  expect(screen.getByText('Waiting for verification')).toBeInTheDocument()
  act(() => (options.callback as (value: string) => void)('refreshed-token'))
  act(() => (options['error-callback'] as () => void)())
  expect(screen.getByText('Waiting for verification')).toBeInTheDocument()
  expect(renderWidget).toHaveBeenCalledTimes(1)
})

it('all waiting widgets initialize when the shared script finishes loading', () => {
  const verify = vi.fn()
  const view = render(
    <>
      <Turnstile siteKey='test-site' onVerify={verify} />
      <Turnstile siteKey='test-site' onVerify={verify} />
    </>
  )
  const renderWidget = vi.fn(() => 'widget')
  window.turnstile = { render: renderWidget, remove: vi.fn() }
  const script = document.querySelector('#cf-turnstile')
  if (!script) throw new Error('Turnstile script was not created')
  fireEvent.load(script)
  expect(renderWidget).toHaveBeenCalledTimes(2)
  view.unmount()
})

it('mounting and expiry never report a successful verification with an empty token', () => {
  let options: Record<string, unknown> = {}
  window.turnstile = {
    render: (_element, value) => {
      options = value
      return 'widget'
    },
    remove: vi.fn(),
  }
  const verify = vi.fn()
  const expire = vi.fn()
  render(<Turnstile siteKey='test-site' onVerify={verify} onExpire={expire} />)
  expect(verify).not.toHaveBeenCalled()
  expect(expire).not.toHaveBeenCalled()
  act(() => (options['expired-callback'] as () => void)())
  expect(verify).not.toHaveBeenCalled()
  expect(expire).toHaveBeenCalledOnce()
})

it('a failed script can be downloaded again after remounting the widget', () => {
  const verify = vi.fn()
  const view = render(
    <Turnstile key='first' siteKey='test-site' onVerify={verify} />
  )
  const failedScript = document.querySelector('#cf-turnstile')
  if (!failedScript) throw new Error('Turnstile script was not created')
  fireEvent.error(failedScript)
  view.rerender(<Turnstile key='retry' siteKey='test-site' onVerify={verify} />)
  const retryScript = document.querySelector('#cf-turnstile')
  if (!retryScript) throw new Error('Turnstile script was not retried')
  expect(retryScript).not.toBe(failedScript)
  const renderWidget = vi.fn(() => 'recovered-widget')
  window.turnstile = { render: renderWidget, remove: vi.fn() }
  fireEvent.load(retryScript)
  expect(renderWidget).toHaveBeenCalledOnce()
})
