export function supportsImageBilling(model: string): boolean {
  const name = model.trim().toLowerCase()
  return name.startsWith('gpt-image-')
}
