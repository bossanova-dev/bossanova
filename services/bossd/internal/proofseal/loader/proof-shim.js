;(() => {
  const PAGE = 'page.html.enc'
  const SEALED = '.enc'
  const VERSION = 1
  const NONCE = 12
  const TAG = 16
  const PLAIN = new Set(['', 'index.html', 'proof-shim.js', 'player.js'])
  const SCRIPTS = new Set(['player.js'])
  const LINKS = ['src', 'href', 'poster', 'data-src', 'data-poster']
  const TYPES = {
    avif: 'image/avif',
    gif: 'image/gif',
    jpeg: 'image/jpeg',
    jpg: 'image/jpeg',
    json: 'application/json',
    mp4: 'video/mp4',
    ndjson: 'application/x-ndjson',
    pdf: 'application/pdf',
    png: 'image/png',
    svg: 'image/svg+xml',
    txt: 'text/plain',
    webm: 'video/webm',
    webp: 'image/webp',
  }

  class KeyError extends Error {}
  class LoadError extends Error {}

  const show = (state) => {
    for (const el of document.querySelectorAll('[data-proof-state]'))
      el.hidden = el.getAttribute('data-proof-state') !== state
  }

  const fragmentKey = () => {
    const k = new URLSearchParams(location.hash.slice(1)).get('k')
    return k && /^[A-Za-z0-9_-]{43}$/.test(k) ? k : null
  }

  const keyBytes = (k) => {
    const raw = atob(`${k.replace(/-/g, '+').replace(/_/g, '/')}=`)
    return Uint8Array.from(raw, (c) => c.charCodeAt(0))
  }

  // The key of the open page. In-page section links rewrite the fragment (player.js through
  // history.replaceState, plain links by navigating), so the key is put back in front of the
  // section and a reload or a copied address still opens the page.
  let opened = null
  const keyed = (href) => {
    const url = new URL(href, location.href)
    const section = url.hash.slice(1)
    if (opened === null || new URLSearchParams(section).has('k')) return url.href
    url.hash = section ? `k=${opened}&${section}` : `k=${opened}`
    return url.href
  }
  for (const method of ['pushState', 'replaceState']) {
    const native = history[method].bind(history)
    history[method] = (state, unused, url) => native(state, unused, url == null ? url : keyed(url))
  }

  const base = new URL('.', document.baseURI)
  const objectURL = (name) => new URL(name.split('/').map(encodeURIComponent).join('/'), base).href

  const objectName = (value) => {
    if (!value || value.startsWith('#')) return null
    let url
    try {
      url = new URL(value, document.baseURI)
    } catch {
      return null
    }
    if (url.origin !== base.origin || !url.pathname.startsWith(base.pathname)) return null
    try {
      return decodeURIComponent(url.pathname.slice(base.pathname.length))
    } catch {
      return null
    }
  }

  const fetchObject = async (name) => {
    const response = await fetch(objectURL(name), {
      credentials: 'omit',
      redirect: 'error',
      referrerPolicy: 'no-referrer',
    })
    if (!response.ok) throw new LoadError(`${name}: ${response.status}`)
    return new Uint8Array(await response.arrayBuffer())
  }

  const open = async (key, name) => {
    const sealed = await fetchObject(name)
    if (sealed.length < 1 + NONCE + TAG || sealed[0] !== VERSION)
      throw new LoadError(`${name}: not a sealed object`)
    try {
      return await crypto.subtle.decrypt(
        {
          name: 'AES-GCM',
          iv: sealed.subarray(1, 1 + NONCE),
          additionalData: new TextEncoder().encode(name),
        },
        key,
        sealed.subarray(1 + NONCE),
      )
    } catch {
      throw new KeyError(`${name}: does not open under this key`)
    }
  }

  const typeOf = (name) => TYPES[name.slice(name.lastIndexOf('.') + 1).toLowerCase()] ?? ''

  const swapReferences = async (root, key) => {
    const blobs = new Map()
    const blobFor = (name) => {
      if (!blobs.has(name))
        blobs.set(
          name,
          open(key, name + SEALED).then(
            (plain) => URL.createObjectURL(new Blob([plain], { type: typeOf(name) })),
            () => null,
          ),
        )
      return blobs.get(name)
    }
    const swaps = []
    for (const el of root.querySelectorAll(LINKS.map((attr) => `[${attr}]`).join(','))) {
      for (const attr of LINKS) {
        const name = objectName(el.getAttribute(attr))
        if (name === null || PLAIN.has(name)) continue
        swaps.push(blobFor(name).then((url) => url && el.setAttribute(attr, url)))
      }
    }
    await Promise.all(swaps)
  }

  const copyAttributes = (from, to) => {
    for (const { name, value } of from.attributes)
      if (!name.toLowerCase().startsWith('on')) to.setAttribute(name, value)
  }

  const render = async (key, html) => {
    const page = new DOMParser().parseFromString(html, 'text/html')
    const scripts = [...page.querySelectorAll('script[src]')]
      .map((script) => objectName(script.getAttribute('src')))
      .filter((name, i, names) => SCRIPTS.has(name) && names.indexOf(name) === i)
    for (const el of page.querySelectorAll('script, meta, base, link')) el.remove()
    await swapReferences(page, key)
    copyAttributes(page.documentElement, document.documentElement)
    copyAttributes(page.body, document.body)
    document.title = page.title
    for (const style of page.head.querySelectorAll('style'))
      document.head.append(document.adoptNode(style))
    document.body.replaceChildren(...[...page.body.childNodes].map((n) => document.adoptNode(n)))
    for (const name of scripts) {
      const script = document.createElement('script')
      script.src = objectURL(name)
      document.body.append(script)
    }
  }

  let busy = false
  const start = async () => {
    if (busy) return
    const k = fragmentKey()
    if (!k) return show('missing-key')
    busy = true
    show('opening')
    try {
      const key = await crypto.subtle.importKey('raw', keyBytes(k), 'AES-GCM', false, ['decrypt'])
      await render(key, new TextDecoder().decode(await open(key, PAGE)))
      opened = k
    } catch (err) {
      busy = false
      show(err instanceof KeyError ? 'missing-key' : 'unavailable')
    }
  }

  // A frame whose src gains its fragment later navigates in place, so a key arriving then opens.
  addEventListener('hashchange', () => {
    if (opened === null) return start()
    if (keyed(location.href) === location.href) return
    try {
      history.replaceState(history.state, '', location.href)
    } catch {
      /* A sandboxed frame has an opaque origin and may not rewrite its URL. */
    }
  })
  start()
})()
