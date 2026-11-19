import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from '@tanstack/react-router'
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { STATUS_QUERY_KEY } from '@/lib/status-query'

import { SignUpForm } from '../components/sign-up-form'

let widgetOptions: Record<string, unknown>[]
let queryClient: QueryClient

beforeEach(() => {
  localStorage.clear()
  widgetOptions = []
  queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  queryClient.setQueryData(STATUS_QUERY_KEY, {
    turnstile_check: true,
    turnstile_site_key: 'test-site',
    email_verification: true,
    oauth_register_enabled: false,
  })
  window.turnstile = {
    render: (element, options) => {
      widgetOptions.push(options)
      const button = document.createElement('button')
      button.type = 'button'
      button.textContent = 'Complete human verification'
      button.addEventListener('click', () =>
        (options.callback as (token: string) => void)(
          `token-${widgetOptions.length}`
        )
      )
      element.appendChild(button)
      return String(widgetOptions.length)
    },
    remove: vi.fn(),
  }
})

afterEach(() => {
  cleanup()
  queryClient.clear()
  delete window.turnstile
  localStorage.clear()
})

async function renderRegistration(captchaEnabled = true) {
  const root = createRootRoute({ component: Outlet })
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({
        getParentRoute: () => root,
        path: '/sign-up',
        component: SignUpForm,
      }),
      createRoute({
        getParentRoute: () => root,
        path: '/sign-in',
        component: () => <div>Sign-in destination</div>,
      }),
    ]),
    history: createMemoryHistory({ initialEntries: ['/sign-up'] }),
  })
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  await user.type(await screen.findByLabelText('Username'), 'new-user')
  await user.type(
    screen.getByLabelText('Password', { exact: true }),
    'securePass-4826'
  )
  await user.type(screen.getByLabelText('Confirm password'), 'securePass-4826')
  await user.type(
    screen.getByLabelText('Email (required for verification)'),
    'user@gmail.com'
  )
  if (captchaEnabled) {
    await user.click(
      screen.getByRole('button', { name: 'Complete human verification' })
    )
  }
  return user
}

it('a failed email request prepares a fresh challenge and prevents concurrent registration', async () => {
  let finish!: (value: { data: { success: boolean; message: string } }) => void
  const get = vi.spyOn(api, 'get').mockReturnValue(
    new Promise((resolve) => {
      finish = resolve
    })
  )
  const post = vi.spyOn(api, 'post')
  const user = await renderRegistration()
  await user.type(screen.getByPlaceholderText('Verification code'), '123456')
  await user.click(screen.getByRole('button', { name: 'Send code' }))
  expect(screen.getByRole('button', { name: 'Create account' })).toBeDisabled()
  expect(post).not.toHaveBeenCalled()
  await waitFor(() => expect(widgetOptions).toHaveLength(2))
  await user.click(
    screen.getByRole('button', { name: 'Complete human verification' })
  )
  expect(screen.getByRole('button', { name: 'Create account' })).toBeDisabled()
  act(() =>
    finish({
      data: {
        success: false,
        message: 'Email provider temporarily unavailable',
      },
    })
  )
  const send = await screen.findByRole('button', { name: 'Send code' })
  await waitFor(() => expect(send).toBeEnabled())
  get.mockResolvedValue({ data: { success: false, message: 'Retry later' } })
  await user.click(send)
  expect(get).toHaveBeenLastCalledWith(
    '/api/verification',
    expect.objectContaining({
      params: { email: 'user@gmail.com', turnstile: 'token-2' },
    })
  )
})

it('email success and registration failure each prepare a fresh token for the next request', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({ data: { success: true } })
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValueOnce({
      data: {
        success: false,
        message: 'Verification code is incorrect or has expired',
      },
    })
    .mockResolvedValue({ data: { success: true } })
  const user = await renderRegistration()
  await user.click(screen.getByRole('button', { name: 'Send code' }))
  await waitFor(() => expect(widgetOptions).toHaveLength(2))
  await user.click(
    screen.getByRole('button', { name: 'Complete human verification' })
  )
  await user.type(screen.getByPlaceholderText('Verification code'), '123456')
  await user.click(screen.getByRole('button', { name: 'Create account' }))
  await waitFor(() => expect(post).toHaveBeenCalledTimes(1))
  expect(post).toHaveBeenLastCalledWith(
    '/api/user/register',
    expect.objectContaining({ turnstile: 'token-2' }),
    expect.objectContaining({ params: { turnstile: 'token-2' } })
  )
  await waitFor(() => expect(widgetOptions).toHaveLength(3))
  await user.click(
    screen.getByRole('button', { name: 'Complete human verification' })
  )
  await user.click(screen.getByRole('button', { name: 'Create account' }))
  await waitFor(() => expect(post).toHaveBeenCalledTimes(2))
  expect(post).toHaveBeenLastCalledWith(
    '/api/user/register',
    expect.objectContaining({ turnstile: 'token-3' }),
    expect.objectContaining({ params: { turnstile: 'token-3' } })
  )
  expect(
    widgetOptions.every((options) => options.action === 'registration')
  ).toBe(true)
  expect(await screen.findByText('Sign-in destination')).toBeInTheDocument()
})

it('registration still succeeds when Turnstile is disabled', async () => {
  queryClient.setQueryData(STATUS_QUERY_KEY, {
    turnstile_check: false,
    email_verification: true,
    oauth_register_enabled: false,
  })
  vi.spyOn(api, 'get').mockResolvedValue({ data: { success: true } })
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true } })
  const user = await renderRegistration(false)
  await user.click(screen.getByRole('button', { name: 'Send code' }))
  await user.type(screen.getByPlaceholderText('Verification code'), '123456')
  await user.click(screen.getByRole('button', { name: 'Create account' }))
  expect(await screen.findByText('Sign-in destination')).toBeInTheDocument()
  expect(post).toHaveBeenCalledOnce()
  expect(widgetOptions).toHaveLength(0)
})

it('submits the QX invitation separately from affiliate codes and keeps it after an error', async () => {
  window.history.replaceState({}, '', '/sign-up?qx_ref=INVITE1234567')
  localStorage.setItem('aff', 'other-affiliate')
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: false, message: 'Retry later' } })
  try {
    const user = await renderRegistration()
    const invite = screen.getByLabelText('QXChat invitation code (optional)')
    expect(invite).toHaveValue('INVITE1234567')
    await user.type(screen.getByPlaceholderText('Verification code'), '123456')
    await user.click(screen.getByRole('button', { name: 'Create account' }))
    await waitFor(() =>
      expect(post).toHaveBeenCalledWith(
        '/api/user/register',
        expect.objectContaining({
          qx_invite_code: 'INVITE1234567',
          aff_code: 'other-affiliate',
        }),
        expect.anything()
      )
    )
    expect(invite).toHaveValue('INVITE1234567')
  } finally {
    window.history.replaceState({}, '', '/')
  }
})
