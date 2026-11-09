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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Check, Save } from 'lucide-react'
import { useEffect, useId, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { MultiSelect } from '@/components/multi-select'
import { Button } from '@/components/ui/button'
import { Field, FieldDescription, FieldLabel } from '@/components/ui/field'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import {
  getModelPricing,
  invalidateModelPricing,
  previewModelPricing,
  saveModelPricing,
  type ModelPricingChange,
} from '@/features/model-pricing/api'
import { formatPricingAmount } from '@/features/model-pricing/currency'
import { PricingAmountInput } from '@/features/model-pricing/pricing-amount-input'
import { generateTaskExprFromConfig } from '@/features/pricing/lib/task-expr'

import { SettingsSection } from '../components/settings-section'
import {
  getVideoBilling,
  saveVideoBilling,
  videoPricingChanges,
  type VideoBillingBindings,
  type VideoBillingConfig,
} from './video-billing'

export function VideoBillingSection() {
  const { t } = useTranslation()
  const client = useQueryClient()
  const query = useQuery({
    queryKey: ['video-billing'],
    queryFn: getVideoBilling,
    refetchOnWindowFocus: false,
  })
  if (query.isPending) return <LoadingState />
  if (query.isError) return <ErrorState onRetry={() => void query.refetch()} />
  return (
    <VideoBillingForm
      key={query.data.version}
      config={query.data}
      onSaved={async (data) => {
        client.setQueryData(['video-billing'], data)
        await invalidateModelPricing(client)
        toast.success(t('Setting updated successfully'))
      }}
    />
  )
}

function VideoBillingForm(props: {
  config: VideoBillingConfig
  onSaved: (data: VideoBillingConfig) => Promise<void>
}) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const channelsId = useId()
  const modelsId = useId()
  const selectedId = useId()
  const priceId = useId()
  const overwriteId = useId()
  const formRef = useRef<HTMLFormElement>(null)
  const [bindings, setBindings] = useState<VideoBillingBindings>(
    props.config.bindings
  )
  const [selected, setSelected] = useState<string[]>(props.config.models)
  const [mode, setMode] = useState('second')
  const [price, setPrice] = useState('')
  const [rates, setRates] = useState<Record<string, string>>(() =>
    Object.fromEntries(
      (props.config.usage_schema.resolution?.enum ?? []).map((value) => [
        value,
        '',
      ])
    )
  )
  const [overwrite, setOverwrite] = useState(false)
  const [plan, setPlan] = useState<{
    changes: ModelPricingChange[]
    skipped: string[]
  } | null>(null)
  const saveBindings = useMutation({
    mutationFn: () =>
      saveVideoBilling({
        bindings,
        expected_version: props.config.version,
      }),
    onSuccess: props.onSaved,
  })
  const preview = useMutation({
    mutationFn: async () => {
      if (!formRef.current?.reportValidity()) {
        return null
      }
      const values = mode === 'request' ? [price] : Object.values(rates)
      for (const value of values) {
        if (
          value.trim() === '' ||
          !Number.isFinite(Number(value)) ||
          Number(value) < 0
        ) {
          throw new Error(
            t('The converted price must be a finite, non-negative number.')
          )
        }
      }
      const nextExpression =
        mode === 'request'
          ? `tier("base", ${Number(price)})`
          : generateTaskExprFromConfig(
              {
                tiers: Object.entries(rates).map(([resolution, amount]) => ({
                  label: resolution,
                  conditions: [{ field: 'resolution', value: resolution }],
                  constant: 0,
                  unitPrices: { seconds: Number(amount) },
                })),
              },
              props.config.usage_schema
            )
      const snapshot = await getModelPricing(selected)
      const nextPlan = videoPricingChanges(
        snapshot,
        selected,
        nextExpression,
        overwrite
      )
      if (nextPlan.changes.length > 0) {
        await previewModelPricing({
          model_name: nextPlan.changes[0].model_name,
          pricing: nextPlan.changes[0].pricing,
        })
      }
      return nextPlan
    },
    onSuccess: setPlan,
  })
  const savePrices = useMutation({
    mutationFn: () => saveModelPricing(plan?.changes ?? []),
    onSuccess: async () => {
      setPlan(null)
      await invalidateModelPricing(client)
      toast.success(t('Model prices saved'))
    },
  })
  useEffect(() => {
    setSelected(props.config.models)
  }, [props.config.models])
  const channelOptions = props.config.channels.map((channel) => ({
    value: String(channel.id),
    label: `${channel.name} (#${channel.id})`,
  }))
  const modelOptions = [
    ...new Set(props.config.channels.flatMap((channel) => channel.models)),
  ].map((name) => ({ value: name, label: name }))
  const dirty =
    JSON.stringify({
      channel_ids: [...bindings.channel_ids].sort((a, b) => a - b),
      models: [...bindings.models].sort(),
    }) !== JSON.stringify(props.config.bindings)
  const busy =
    savePrices.isPending || preview.isPending || saveBindings.isPending

  return (
    <SettingsSection title={t('Video billing templates')}>
      <form
        ref={formRef}
        onSubmit={(event) => event.preventDefault()}
        className='min-w-0 space-y-6'
      >
        <div className='grid min-w-0 gap-4 lg:grid-cols-2'>
          <Field>
            <FieldLabel htmlFor={channelsId}>{t('Video channels')}</FieldLabel>
            <MultiSelect
              id={channelsId}
              options={channelOptions}
              selected={bindings.channel_ids.map(String)}
              onChange={(values) =>
                setBindings({ ...bindings, channel_ids: values.map(Number) })
              }
              disabled={busy}
              maxVisibleChips={5}
            />
          </Field>
          <Field>
            <FieldLabel htmlFor={modelsId}>
              {t('Additional video models')}
            </FieldLabel>
            <MultiSelect
              id={modelsId}
              options={modelOptions}
              selected={bindings.models}
              onChange={(models) => setBindings({ ...bindings, models })}
              allowCreate
              disabled={busy}
              maxVisibleChips={5}
            />
          </Field>
        </div>
        <Button
          type='button'
          variant='outline'
          disabled={!dirty || busy}
          onClick={() => saveBindings.mutate()}
        >
          <Save aria-hidden='true' />
          {saveBindings.isPending
            ? t('Saving...')
            : t('Save video associations')}
        </Button>

        <div className='min-w-0 space-y-4 border-t pt-5'>
          <Field>
            <FieldLabel htmlFor={selectedId}>{t('Models to price')}</FieldLabel>
            <MultiSelect
              id={selectedId}
              options={props.config.models.map((name) => ({
                value: name,
                label: name,
              }))}
              selected={selected}
              onChange={setSelected}
              disabled={busy || dirty}
              maxVisibleChips={6}
            />
            <FieldDescription>
              {t(
                'Existing model prices are preserved unless replacement is enabled.'
              )}
            </FieldDescription>
          </Field>
          <Tabs value={mode} onValueChange={setMode}>
            <TabsList>
              <TabsTrigger value='second' disabled={busy}>
                {t('Per second by resolution')}
              </TabsTrigger>
              <TabsTrigger value='request' disabled={busy}>
                {t('Per request')}
              </TabsTrigger>
            </TabsList>
          </Tabs>
          {mode === 'request' ? (
            <Field className='sm:max-w-sm'>
              <FieldLabel htmlFor={priceId}>{t('USD per request')}</FieldLabel>
              <PricingAmountInput
                id={priceId}
                value={price}
                onChange={setPrice}
                required
                min={0}
                disabled={busy}
              />
            </Field>
          ) : (
            <div className='grid min-w-0 gap-4 sm:grid-cols-3'>
              {Object.entries(rates).map(([resolution, amount]) => (
                <Field key={resolution}>
                  <FieldLabel htmlFor={`${priceId}-${resolution}`}>
                    {resolution} (USD/{t('second')})
                  </FieldLabel>
                  <PricingAmountInput
                    id={`${priceId}-${resolution}`}
                    value={amount}
                    onChange={(value) =>
                      setRates({ ...rates, [resolution]: value })
                    }
                    required
                    min={0}
                    disabled={busy}
                  />
                </Field>
              ))}
            </div>
          )}
          <div className='flex items-center gap-3'>
            <Switch
              id={overwriteId}
              checked={overwrite}
              onCheckedChange={setOverwrite}
              disabled={busy}
            />
            <Label htmlFor={overwriteId}>
              {t('Replace existing prices for selected models')}
            </Label>
          </div>
          <Button
            type='button'
            disabled={busy || dirty || selected.length === 0}
            onClick={() => preview.mutate()}
          >
            <Check aria-hidden='true' />
            {preview.isPending ? t('Loading...') : t('Review video prices')}
          </Button>
        </div>
      </form>
      <ConfirmDialog
        open={plan !== null}
        onOpenChange={(open) => {
          if (!open && !savePrices.isPending) setPlan(null)
        }}
        title={t('Apply video prices?')}
        desc={t(
          'Apply to {{applied}} models; preserve {{skipped}} existing prices.',
          {
            applied: plan?.changes.length ?? 0,
            skipped: plan?.skipped.length ?? 0,
          }
        )}
        confirmText={t('Save model prices')}
        disabled={!plan?.changes.length}
        isLoading={savePrices.isPending}
        handleConfirm={() => savePrices.mutate()}
      >
        <div className='max-h-64 space-y-2 overflow-auto text-xs'>
          <p className='break-words'>
            {plan?.changes.map((change) => change.model_name).join(', ')}
          </p>
          <dl className='grid grid-cols-[1fr_auto] gap-x-4 gap-y-2'>
            {(mode === 'request'
              ? [[t('Per request'), price]]
              : Object.entries(rates)
            ).map(([label, amount]) => (
              <div key={label} className='contents'>
                <dt>{label}</dt>
                <dd>
                  {formatPricingAmount(Number(amount))}
                  {mode === 'second' ? `/${t('second')}` : ''}
                </dd>
              </div>
            ))}
          </dl>
          {savePrices.isError && (
            <p role='alert'>{t('Failed to save model pricing')}</p>
          )}
        </div>
      </ConfirmDialog>
    </SettingsSection>
  )
}
