export function supportsImageBilling(model: string): boolean {
  const name = model.trim().toLowerCase()
  return name.startsWith('gpt-image-')
}

export type MediaBillingUnit = 'image' | 'second'

// mediaBillingUnit mirrors database.MediaBillingUnit: Grok Imagine models are billed
// per image or per generated video second instead of per token.
export function mediaBillingUnit(model: string): MediaBillingUnit | '' {
  const name = (model.trim().toLowerCase().split('/').pop() ?? '')
  if (!name.startsWith('grok-imagine')) return ''
  return name.startsWith('grok-imagine-video') ? 'second' : 'image'
}
