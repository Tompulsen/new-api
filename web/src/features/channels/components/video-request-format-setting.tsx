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
import { useFormContext } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

import type { ChannelFormValues } from '../lib/channel-form'
import { supportsVideoRequestFormat } from '../lib/video-request-format'

export function VideoRequestFormatSetting(props: { disabled?: boolean }) {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormValues>()
  const [type, taskPluginKey, taskExtendPluginKeys] = form.watch([
    'type',
    'task_plugin_key',
    'task_extend_plugin_keys',
  ])
  if (
    !supportsVideoRequestFormat({
      type,
      task_plugin_key: taskPluginKey,
      task_extend_plugin_keys: taskExtendPluginKeys,
    })
  ) {
    return null
  }
  const options = [
    { value: 'openai', label: t('Standard OpenAI Video') },
    { value: 'json', label: t('JSON + image URLs') },
  ]

  return (
    <FormField
      control={form.control}
      name='video_request_format'
      render={({ field }) => (
        <FormItem className='min-w-0 space-y-2 px-4 py-3'>
          <FormLabel>{t('Video request format')}</FormLabel>
          <Select
            items={options}
            value={field.value || 'openai'}
            onValueChange={(value) => value !== null && field.onChange(value)}
            disabled={props.disabled}
          >
            <FormControl>
              <SelectTrigger className='w-full' onBlur={field.onBlur}>
                <SelectValue />
              </SelectTrigger>
            </FormControl>
            <SelectContent>
              <SelectGroup>
                {options.map((option) => (
                  <SelectItem key={option.value} value={option.value}>
                    {option.label}
                  </SelectItem>
                ))}
              </SelectGroup>
            </SelectContent>
          </Select>
          <FormDescription>
            {field.value === 'json'
              ? t(
                  'Upload local reference images through /v1/files, then submit JSON with image URLs.'
                )
              : t(
                  'Use the standard OpenAI Video request format for this channel.'
                )}
          </FormDescription>
          <FormMessage />
        </FormItem>
      )}
    />
  )
}
