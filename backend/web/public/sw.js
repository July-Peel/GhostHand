const cacheName = 'ghosthand-static-v1'

self.addEventListener('install', event => {
  event.waitUntil(self.skipWaiting())
})

self.addEventListener('activate', event => {
  event.waitUntil((async () => {
    for (const name of await caches.keys()) {
      if (name.startsWith('ghosthand-static-') && name !== cacheName) await caches.delete(name)
    }
    await self.clients.claim()
  })())
})

self.addEventListener('fetch', event => {
  const url = new URL(event.request.url)
  if (event.request.method !== 'GET' || url.origin !== self.location.origin || !url.pathname.startsWith('/assets/')) return
  event.respondWith((async () => {
    const cache = await caches.open(cacheName)
    const existing = await cache.match(event.request)
    if (existing) return existing
    const response = await fetch(event.request)
    if (response.ok) {
      await cache.put(event.request, response.clone())
      const entries = await cache.keys()
      for (const entry of entries.slice(0, Math.max(0, entries.length - 24))) await cache.delete(entry)
    }
    return response
  })())
})
