import { useTranslation } from 'react-i18next'
import { Info } from 'lucide-react'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { formatImageStudioQuota } from '@/lib/imageStudioQuota'

// ImageBillingCost 展示按张计费的用户金额与计费依据;count 是落库的计费张数。
export function ImageBillingCost({ count = 0, unitPrice = 0, userBilled, accountBilled }: {
  count?: number
  unitPrice?: number
  userBilled: number
  accountBilled?: number
}) {
  const { t } = useTranslation()
  const values = { count, price: formatImageStudioQuota(unitPrice), total: formatImageStudioQuota(userBilled) }
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className="inline-flex items-center gap-1.5 rounded-md px-1.5 py-1 text-[13px] font-semibold tabular-nums text-emerald-600 hover:bg-muted/60 dark:text-emerald-400">
          {formatImageStudioQuota(userBilled)}<Info className="size-3.5 text-muted-foreground" />
        </button>
      </TooltipTrigger>
      <TooltipContent className="max-w-80 space-y-1.5 p-3 text-xs">
        <p className="font-semibold">{t('settings.pricing.imageBilling.perImage')}</p>
        <p>{t('usage.imageBillingFormula', values)}</p>
        {count === 0 ? <p>{t('usage.imageBillingNoCharge')}</p> : null}
        {accountBilled != null ? <p className="border-t pt-1.5">{t('usage.imageUpstreamCost', { amount: formatImageStudioQuota(accountBilled) })}</p> : null}
      </TooltipContent>
    </Tooltip>
  )
}
