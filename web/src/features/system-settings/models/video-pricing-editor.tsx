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
import { useId, useState, type Dispatch, type SetStateAction } from 'react'
import { useTranslation } from 'react-i18next'

import { Field, FieldLabel } from '@/components/ui/field'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import type { PricingCurrency } from '@/features/model-pricing/currency'
import { PricingAmountInput } from '@/features/model-pricing/pricing-amount-input'
import {
  combineBillingExpr,
  splitBillingExprAndRequestRules,
} from '@/features/pricing/lib/billing-expr'
import { compileBillingExpression } from '@/features/pricing/lib/billing-expression/parser'
import type { BillingUsageSchema } from '@/features/pricing/types'

import { TaskUsagePricingEditor } from './task-usage-pricing-editor'

export function VideoPricingEditor(props: {
  expression: string
  onChange: Dispatch<SetStateAction<string>>
  schema: BillingUsageSchema
  currency: PricingCurrency
}) {
  const { t } = useTranslation()
  const id = useId()
  const split = splitBillingExprAndRequestRules(props.expression)
  const compiled = compileBillingExpression(split.billingExpr)
  const root = compiled.status === 'ready' ? compiled.ast : undefined
  const amount =
    root?.kind === 'call' &&
    root.name === 'tier' &&
    root.args[1]?.kind === 'literal' &&
    typeof root.args[1].value === 'number'
      ? root.args[1].value
      : undefined
  const [unit, setUnit] = useState(amount === undefined ? 'second' : 'request')
  const [requestPrice, setRequestPrice] = useState(
    amount === undefined ? '' : String(amount)
  )
  const [secondExpression, setSecondExpression] = useState(
    amount === undefined ? props.expression : ''
  )
  const secondSplit = splitBillingExprAndRequestRules(secondExpression)
  const requestExpression = (price: string) =>
    price.trim() === ''
      ? ''
      : combineBillingExpr(
          `tier("base", ${Number(price)})`,
          split.requestRuleExpr
        )

  return (
    <Tabs
      value={unit}
      onValueChange={(value) => {
        setUnit(value)
        props.onChange(
          value === 'request'
            ? requestExpression(requestPrice)
            : secondExpression
        )
      }}
    >
      <TabsList className='max-w-full flex-wrap group-data-horizontal/tabs:h-auto'>
        <TabsTrigger value='second'>
          {t('Per second by resolution')}
        </TabsTrigger>
        <TabsTrigger value='request'>{t('Per request')}</TabsTrigger>
      </TabsList>
      <TabsContent value='request'>
        <Field>
          <FieldLabel htmlFor={id}>
            {t('Per request')} ({props.currency.label})
          </FieldLabel>
          <PricingAmountInput
            id={id}
            currency={props.currency}
            value={requestPrice}
            required
            min={0}
            onChange={(price) => {
              setRequestPrice(price)
              props.onChange(requestExpression(price))
            }}
          />
        </Field>
      </TabsContent>
      <TabsContent value='second' className='min-w-0'>
        <TaskUsagePricingEditor
          currency={props.currency}
          billingExpr={secondSplit.billingExpr}
          requestRuleExpr={secondSplit.requestRuleExpr}
          usageSchema={props.schema}
          onBillingExprChange={(value) => {
            const update = (current: string) =>
              combineBillingExpr(
                value,
                splitBillingExprAndRequestRules(current).requestRuleExpr
              )
            setSecondExpression(update)
            props.onChange(update)
          }}
          onRequestRuleExprChange={(rules) => {
            const update = (current: string) =>
              combineBillingExpr(
                splitBillingExprAndRequestRules(current).billingExpr,
                rules
              )
            setSecondExpression(update)
            props.onChange(update)
          }}
        />
      </TabsContent>
    </Tabs>
  )
}
