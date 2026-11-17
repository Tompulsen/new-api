import { act, fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { afterEach, expect, it, vi } from 'vitest'

import { Turnstile } from '../turnstile'

afterEach(() => {
  vi.useRealTimers()
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

it('offers a keyboard accessible retry after a failed script and only verifies a fresh challenge', async () => {
  const user = userEvent.setup()
  const verify = vi.fn()
  const expire = vi.fn()
  render(<Turnstile siteKey='test-site' onVerify={verify} onExpire={expire} />)
  const failedScript = document.querySelector('#cf-turnstile')
  if (!failedScript) throw new Error('Turnstile script was not created')
  fireEvent.error(failedScript)
  expect(verify).not.toHaveBeenCalled()
  expect(expire).toHaveBeenCalled()
  const retry = screen.getByRole('button', { name: 'Retry' })
  expect(retry).toBeVisible()
  retry.focus()
  await user.keyboard('{Enter}')
  const retryScript = document.querySelector('#cf-turnstile')
  if (!retryScript) throw new Error('Turnstile script was not retried')
  expect(retryScript).not.toBe(failedScript)
  let options: Record<string, unknown> = {}
  window.turnstile = {
    render: (_element, value) => {
      options = value
      return 'recovered-widget'
    },
    remove: vi.fn(),
  }
  fireEvent.load(retryScript)
  expect(verify).not.toHaveBeenCalled()
  act(() => (options.callback as (value: string) => void)('fresh-token'))
  expect(verify).toHaveBeenCalledWith('fresh-token')
  expect(
    screen.queryByRole('button', { name: 'Retry' })
  ).not.toBeInTheDocument()
})

it('a stalled script stops initializing and allows a new download', () => {
  vi.useFakeTimers()
  const verify = vi.fn()
  render(<Turnstile siteKey='test-site' onVerify={verify} />)
  expect(screen.getByRole('status')).toHaveTextContent(
    'Please wait a moment, human check is initializing...'
  )
  const stalledScript = document.querySelector('#cf-turnstile')
  act(() => vi.advanceTimersByTime(15000))
  expect(verify).not.toHaveBeenCalled()
  fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
  expect(document.querySelector('#cf-turnstile')).not.toBe(stalledScript)
})

it('a provider render error can be retried without accepting an empty token', () => {
  const verify = vi.fn()
  const expire = vi.fn()
  const renderWidget = vi
    .fn()
    .mockImplementationOnce(() => {
      throw new Error('Verification provider is not ready')
    })
    .mockReturnValue('recovered-widget')
  window.turnstile = { render: renderWidget, remove: vi.fn() }
  render(<Turnstile siteKey='test-site' onVerify={verify} onExpire={expire} />)
  expect(expire).toHaveBeenCalled()
  fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
  expect(verify).not.toHaveBeenCalled()
  expect(
    screen.queryByRole('button', { name: 'Retry' })
  ).not.toBeInTheDocument()
})

it('a rendered widget that never responds stays visible, times out and retries with a fresh challenge', () => {
  vi.useFakeTimers()
  const callbacks: Record<string, unknown>[] = []
  window.turnstile = {
    render: (_element, options) => {
      callbacks.push(options)
      return String(callbacks.length)
    },
    remove: vi.fn(),
  }
  const verify = vi.fn()
  render(<Turnstile siteKey='test-site' onVerify={verify} />)
  expect(screen.getByRole('status')).toBeVisible()
  act(() => vi.advanceTimersByTime(15000))
  expect(screen.getByRole('alert')).toBeVisible()
  fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
  expect(screen.getByRole('status')).toBeVisible()
  act(() => (callbacks[0].callback as (token: string) => void)('stale-token'))
  expect(verify).not.toHaveBeenCalled()
  act(() => (callbacks[1].callback as (token: string) => void)('fresh-token'))
  expect(verify).toHaveBeenCalledWith('fresh-token')
  act(() => vi.advanceTimersByTime(15000))
  expect(screen.queryByRole('status')).not.toBeInTheDocument()
  expect(screen.queryByRole('alert')).not.toBeInTheDocument()
})

it('an interactive challenge allows time to answer and detects a stalled result afterwards', () => {
  vi.useFakeTimers()
  let options: Record<string, unknown> = {}
  window.turnstile = {
    render: (_element, value) => {
      options = value
      return 'interactive-widget'
    },
    remove: vi.fn(),
  }
  const verify = vi.fn()
  const expire = vi.fn()
  render(<Turnstile siteKey='test-site' onVerify={verify} onExpire={expire} />)
  act(() => (options['before-interactive-callback'] as () => void)())
  act(() => vi.advanceTimersByTime(30000))
  expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  expect(expire).not.toHaveBeenCalled()
  act(() => (options['after-interactive-callback'] as () => void)())
  expect(screen.getByRole('status')).toBeVisible()
  act(() => vi.advanceTimersByTime(15000))
  expect(screen.getByRole('button', { name: 'Retry' })).toBeVisible()
  expect(verify).not.toHaveBeenCalled()
})

it('an expired token shows progress again and an unsupported browser offers recovery', () => {
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
  act(() => (options.callback as (token: string) => void)('valid-token'))
  expect(screen.queryByRole('status')).not.toBeInTheDocument()
  act(() => (options['expired-callback'] as () => void)())
  expect(screen.getByRole('status')).toBeVisible()
  expect(expire).toHaveBeenCalledOnce()
  act(() => (options['unsupported-callback'] as () => void)())
  expect(screen.getByRole('alert')).toBeVisible()
  expect(screen.getByRole('button', { name: 'Retry' })).toBeVisible()
  expect(expire).toHaveBeenCalledTimes(2)
})

it('a completed challenge stays verified when the interaction-ended callback arrives afterwards', () => {
  vi.useFakeTimers()
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
  act(() => (options['before-interactive-callback'] as () => void)())
  act(() => (options.callback as (token: string) => void)('valid-token'))
  act(() => (options['after-interactive-callback'] as () => void)())
  act(() => vi.advanceTimersByTime(15000))
  expect(verify).toHaveBeenCalledWith('valid-token')
  expect(expire).not.toHaveBeenCalled()
  expect(screen.queryByRole('status')).not.toBeInTheDocument()
  expect(screen.queryByRole('alert')).not.toBeInTheDocument()
})
