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
import { CHANNEL_TYPE_NEW_API, CHANNEL_TYPE_TASK_PLUGIN } from '../constants'

export function supportsVideoRequestFormat(values: {
  type: number
  task_plugin_key?: string
  task_extend_plugin_keys?: string[]
}): boolean {
  if (values.type === 1 || values.type === 55) return true
  if (values.type === CHANNEL_TYPE_TASK_PLUGIN) {
    return values.task_plugin_key === 'sora'
  }
  return (
    values.type === CHANNEL_TYPE_NEW_API &&
    (values.task_plugin_key === 'sora' ||
      values.task_extend_plugin_keys?.includes('sora') === true)
  )
}
