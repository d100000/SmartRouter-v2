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
import { useEffect, useRef, type ChangeEvent } from 'react'
import { useFormContext } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { DEFAULT_LOGO } from '@/lib/constants'

type LogoUploadFieldProps = {
  disabled: boolean
  isReading: boolean
  onReadingChange: (reading: boolean) => void
}

function readLogoImage(file: File): Promise<{
  dataUrl: string
  width: number
  height: number
}> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.addEventListener(
      'error',
      () => reject(new Error('Image read failed')),
      { once: true }
    )
    reader.addEventListener(
      'abort',
      () => reject(new Error('Image read cancelled')),
      { once: true }
    )
    reader.addEventListener(
      'load',
      () => {
        if (typeof reader.result !== 'string') {
          reject(new Error('Invalid image data'))
          return
        }
        const dataUrl = reader.result
        const image = new Image()
        image.addEventListener(
          'error',
          () => reject(new Error('Invalid image data')),
          { once: true }
        )
        image.addEventListener(
          'load',
          () =>
            resolve({
              dataUrl,
              width: image.naturalWidth,
              height: image.naturalHeight,
            }),
          { once: true }
        )
        image.src = dataUrl
      },
      { once: true }
    )
    reader.readAsDataURL(file)
  })
}

export function LogoUploadField(props: LogoUploadFieldProps) {
  const { t } = useTranslation()
  const form = useFormContext<{ Logo?: string }>()
  const inputRef = useRef<HTMLInputElement>(null)
  const uploadId = useRef(0)

  useEffect(
    () => () => {
      uploadId.current += 1
    },
    []
  )

  async function handleFileChange(event: ChangeEvent<HTMLInputElement>) {
    const file = event.currentTarget.files?.[0]
    event.currentTarget.value = ''
    if (!file || props.disabled || props.isReading) return

    form.clearErrors('Logo')
    if (!['image/png', 'image/jpeg', 'image/webp'].includes(file.type)) {
      form.setError('Logo', {
        message: t('Choose a PNG, JPG, or WebP image'),
      })
      return
    }
    if (file.size > 512 * 1024) {
      form.setError('Logo', {
        message: t('Logo image must be 512 KB or smaller'),
      })
      return
    }

    const currentUpload = ++uploadId.current
    props.onReadingChange(true)
    try {
      const image = await readLogoImage(file)
      if (currentUpload !== uploadId.current) return
      if (
        image.width < 1 ||
        image.height < 1 ||
        image.width > 2048 ||
        image.height > 2048
      ) {
        form.setError('Logo', {
          message: t('Logo dimensions must not exceed 2048 × 2048 pixels'),
        })
        return
      }
      form.setValue('Logo', image.dataUrl, {
        shouldDirty: true,
        shouldValidate: true,
      })
    } catch {
      if (currentUpload !== uploadId.current) return
      form.setError('Logo', {
        message: t(
          'Unable to read this image. Choose a valid PNG, JPG, or WebP file.'
        ),
      })
    } finally {
      if (currentUpload === uploadId.current) props.onReadingChange(false)
    }
  }

  return (
    <FormField
      control={form.control}
      name='Logo'
      render={({ field }) => (
        <FormItem>
          <FormLabel>{t('Logo image')}</FormLabel>
          <div
            className='flex min-w-0 items-center gap-3'
            aria-busy={props.isReading}
          >
            <div className='bg-muted/20 size-16 shrink-0 rounded-lg border p-2'>
              <img
                src={field.value || DEFAULT_LOGO}
                alt={t('Logo preview')}
                className='size-full object-contain'
                onError={(event) => {
                  if (
                    event.currentTarget.getAttribute('src') !== DEFAULT_LOGO
                  ) {
                    event.currentTarget.src = DEFAULT_LOGO
                  }
                }}
              />
            </div>
            <div className='flex min-w-0 flex-wrap gap-2'>
              <input
                ref={inputRef}
                type='file'
                accept='image/png,image/jpeg,image/webp'
                aria-label={t('Choose logo image')}
                hidden
                disabled={props.disabled || props.isReading}
                onChange={handleFileChange}
              />
              <FormControl>
                <Button
                  ref={field.ref}
                  type='button'
                  size='sm'
                  variant='outline'
                  aria-label={
                    field.value ? t('Replace image') : t('Upload image')
                  }
                  disabled={props.disabled || props.isReading}
                  onBlur={field.onBlur}
                  onClick={() => inputRef.current?.click()}
                >
                  {field.value ? t('Replace image') : t('Upload image')}
                </Button>
              </FormControl>
              <Button
                type='button'
                size='sm'
                variant='ghost'
                disabled={!field.value || props.disabled || props.isReading}
                onClick={() => {
                  form.clearErrors('Logo')
                  field.onChange('')
                }}
              >
                {t('Use default logo')}
              </Button>
            </div>
          </div>
          <FormDescription>
            {t(
              'PNG, JPG or WebP. Up to 512 KB and 2048 × 2048 pixels. Save changes to apply.'
            )}
          </FormDescription>
          {props.isReading && (
            <p role='status' className='text-muted-foreground text-sm'>
              {t('Reading image…')}
            </p>
          )}
          <FormMessage />
        </FormItem>
      )}
    />
  )
}
