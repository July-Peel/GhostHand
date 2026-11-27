export const maxChatImages = 4
const maxImageBytes = 5 * 1024 * 1024
const maxImageSide = 2560
const formats: Record<string, string> = {
  png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg', webp: 'image/webp', gif: 'image/gif',
}
export const acceptedChatImages = 'image/png,image/jpeg,image/webp,image/gif'

export type PendingImage = { file: File; preview: string }

export async function prepareChatImage(file: File): Promise<PendingImage> {
  const format = file.type || formats[file.name.split('.').pop()?.toLowerCase() ?? '']
  if (!acceptedChatImages.split(',').includes(format)) throw new Error('请选择 PNG、JPEG、GIF 或 WebP 图片。')
  if (file.size > maxImageBytes) throw new Error('单张图片不能超过 5 MB。')

  const bitmap = await createImageBitmap(file).catch(() => { throw new Error('无法读取这张图片。') })
  try {
    const edge = Math.max(bitmap.width, bitmap.height)
    if (format === 'image/webp' || edge > maxImageSide) {
      const scale = Math.min(1, maxImageSide / edge)
      const canvas = document.createElement('canvas')
      canvas.width = Math.max(1, Math.round(bitmap.width * scale))
      canvas.height = Math.max(1, Math.round(bitmap.height * scale))
      const ctx = canvas.getContext('2d', { alpha: false })
      if (!ctx) throw new Error('无法转换这张图片。')
      ctx.fillStyle = '#fff'
      ctx.fillRect(0, 0, canvas.width, canvas.height)
      ctx.drawImage(bitmap, 0, 0, canvas.width, canvas.height)
      const blob = await new Promise<Blob | null>(resolve => canvas.toBlob(resolve, 'image/jpeg', 0.9))
      if (!blob) throw new Error('无法转换这张图片。')
      if (blob.size > maxImageBytes) throw new Error('转换后的图片超过 5 MB，请选择更小的图片。')
      file = new File([blob], (file.name.replace(/\.[^.]+$/, '') || '图片') + '.jpg', { type: 'image/jpeg' })
    } else if (file.type !== format) {
      file = new File([file], file.name, { type: format })
    }
  } finally {
    bitmap.close()
  }

  const preview = await new Promise<string>((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result))
    reader.onerror = () => reject(new Error('无法预览这张图片。'))
    reader.readAsDataURL(file)
  })
  return { file, preview }
}
