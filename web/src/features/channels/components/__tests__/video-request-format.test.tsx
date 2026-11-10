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
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useForm } from 'react-hook-form'
import { afterEach, expect, it } from 'vitest'

import { Button } from '@/components/ui/button'
import { Form } from '@/components/ui/form'

import { getChannelConfigurationState } from '../../lib/channel-configuration'
import {
  buildSettingJSON,
  CHANNEL_FORM_DEFAULT_VALUES,
  transformChannelToFormDefaults,
  transformFormDataToCreatePayload,
  transformFormDataToUpdatePayload,
  type ChannelFormValues,
} from '../../lib/channel-form'
import { channelSchema } from '../../types'
import { VideoRequestFormatSetting } from '../video-request-format-setting'

afterEach(cleanup)

function SettingsForm(props: {
  values?: Partial<ChannelFormValues>
  disabled?: boolean
  onSave: (setting: string) => void
}) {
  const form = useForm<ChannelFormValues>({
    defaultValues: { ...CHANNEL_FORM_DEFAULT_VALUES, ...props.values },
  })
  return (
    <Form {...form}>
      <form
        onSubmit={form.handleSubmit((values) =>
          props.onSave(buildSettingJSON(values))
        )}
      >
        <VideoRequestFormatSetting disabled={props.disabled} />
        <Button type='submit'>Save</Button>
      </form>
    </Form>
  )
}

it('saves JSON video transport and restores standard mode through an accessible selector', async () => {
  const saved: string[] = []
  const user = userEvent.setup()
  render(<SettingsForm onSave={(value) => saved.push(value)} />)
  const selector = screen.getByRole('combobox', {
    name: 'Video request format',
  })
  expect(selector).toHaveTextContent('Standard OpenAI Video')
  await user.click(selector)
  await user.click(screen.getByRole('option', { name: 'JSON + image URLs' }))
  await user.click(screen.getByRole('button', { name: 'Save' }))
  expect(JSON.parse(saved[0]).video_request_format).toBe('json')
  await user.click(selector)
  await user.click(
    screen.getByRole('option', { name: 'Standard OpenAI Video' })
  )
  await user.click(screen.getByRole('button', { name: 'Save' }))
  expect(JSON.parse(saved[1])).not.toHaveProperty('video_request_format')
})

it.each([
  { type: 1 },
  { type: 55 },
  { type: 61, task_plugin_key: 'sora' },
  { type: 60, task_extend_plugin_keys: ['sora'] },
])(
  'preserves JSON video transport on create, update and reload (%j)',
  (binding) => {
    const channel = channelSchema.parse({
      id: 1,
      name: 'Video',
      key: '',
      status: 1,
      created_time: 0,
      test_time: 0,
      response_time: 0,
      balance_updated_time: 0,
      ...binding,
      setting: JSON.stringify({
        video_request_format: 'json',
        task_plugin_key: binding.task_plugin_key,
        task_extend_plugin_keys:
          'task_extend_plugin_keys' in binding
            ? binding.task_extend_plugin_keys
            : undefined,
      }),
    })
    const values = transformChannelToFormDefaults(channel)
    for (const payload of [
      transformFormDataToCreatePayload(values).channel,
      transformFormDataToUpdatePayload(values, channel.id),
    ]) {
      expect(JSON.parse(payload.setting as string).video_request_format).toBe(
        'json'
      )
      expect(
        transformChannelToFormDefaults({
          ...channel,
          setting: payload.setting as string,
        }).video_request_format
      ).toBe('json')
    }
    expect(
      getChannelConfigurationState(values, {}, true).blocks.requestProcessing
    ).toBe('configured')
  }
)

it('hides video transport for other providers and omits stale JSON selections', async () => {
  const saved: string[] = []
  render(
    <SettingsForm
      values={{ type: 14, video_request_format: 'json' }}
      onSave={(value) => saved.push(value)}
    />
  )
  expect(screen.queryByRole('combobox')).not.toBeInTheDocument()
  await userEvent.setup().click(screen.getByRole('button', { name: 'Save' }))
  expect(JSON.parse(saved[0])).not.toHaveProperty('video_request_format')
})

it('locks the video transport selector when sensitive settings are read-only', async () => {
  render(
    <SettingsForm
      disabled
      values={{ video_request_format: 'json' }}
      onSave={() => undefined}
    />
  )
  const selector = screen.getByRole('combobox', {
    name: 'Video request format',
  })
  expect(selector).toBeDisabled()
  await userEvent.setup().click(selector)
  expect(screen.queryByRole('option')).not.toBeInTheDocument()
})
