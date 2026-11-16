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
import { zodResolver } from '@hookform/resolvers/zod'
import { useEffect } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import type { z } from 'zod'

import { Dialog } from '@/components/dialog'
import { PasswordInput } from '@/components/password-input'
import { Button } from '@/components/ui/button'
import { Form, FormControl, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Spinner } from '@/components/ui/spinner'
import { loginFormSchema } from '@/features/auth/constants'
import { bindQXChatIdentity } from '@/features/auth/qxchat'
import { SecureVerificationDialog } from '@/features/auth/secure-verification'

import { useAccountSecurity } from '../../hooks/use-account-security'

interface QXChatBindDialogProps {
  open: boolean
  passwordEncryptionEnabled: boolean
  onOpenChange: (open: boolean) => void
  onSuccess: () => void
}

export function QXChatBindDialog(props: QXChatBindDialogProps) {
  const { t } = useTranslation()
  const security = useAccountSecurity()
  const form = useForm<z.infer<typeof loginFormSchema>>({
    resolver: zodResolver(loginFormSchema),
    defaultValues: { username: '', password: '' },
  })
  const reset = form.reset
  const cancel = security.cancel
  useEffect(() => {
    reset({ username: '', password: '' })
  }, [reset, security.sessionKey])
  useEffect(() => {
    if (!props.open) {
      cancel()
      reset({ username: '', password: '' })
    }
  }, [props.open, cancel, reset])

  const handleOpenChange = (open: boolean) => {
    if (!open) {
      security.cancel()
      form.reset({ username: '', password: '' })
    }
    props.onOpenChange(open)
  }
  const submit = async (values: z.infer<typeof loginFormSchema>) => {
    const identifier = values.username.trim().toLowerCase()
    const result = await security.run(async (signal) => {
      const proof = await security.verify({
        scope: 'account.binding.bind',
        context: { provider: 'qxchat', code: identifier },
      }, signal)
      return bindQXChatIdentity(identifier, values.password, props.passwordEncryptionEnabled, proof, signal)
    })
    if (!result) return
    toast.success(t('QXChat account connected'))
    handleOpenChange(false)
    props.onSuccess()
  }

  return (
    <>
      <Dialog
        open={props.open && !security.showVerification}
        onOpenChange={handleOpenChange}
        title={t('Connect QXChat account')}
        description={t('Connect your QXChat account to this New API account. Future sign-ins use this New API account.')}
        contentClassName='sm:max-w-md'
        footer={
          <>
            <Button type='button' variant='outline' onClick={() => handleOpenChange(false)}>{t('Cancel')}</Button>
            <Button type='submit' form='qxchat-bind-form' disabled={security.pending}>
              {security.pending && <Spinner data-icon='inline-start' />}
              {t('Connect QXChat account')}
            </Button>
          </>
        }
      >
        <Form {...form}>
          <form id='qxchat-bind-form' onSubmit={form.handleSubmit(submit)} className='space-y-4'>
            <FormField control={form.control} name='username' render={({ field }) => (
              <FormItem>
                <FormLabel>{t('QXChat username or email')}</FormLabel>
                <FormControl><Input {...field} autoComplete='username' maxLength={255} disabled={security.pending} autoFocus /></FormControl>
                <FormMessage />
              </FormItem>
            )} />
            <FormField control={form.control} name='password' render={({ field }) => (
              <FormItem>
                <FormLabel>{t('QXChat password')}</FormLabel>
                <FormControl><PasswordInput {...field} autoComplete='current-password' maxLength={128} disabled={security.pending} /></FormControl>
                <FormMessage />
              </FormItem>
            )} />
          </form>
        </Form>
      </Dialog>
      {security.showVerification && <SecureVerificationDialog {...security.verificationDialogProps} />}
    </>
  )
}
