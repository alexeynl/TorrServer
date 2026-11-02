// Embedded subtitle tracks of mkv files are served by TorrServer in windows:
//   GET {url}&from=SEC&to=SEC - the lines that are shown within this window (ASS with the original
//   styling, or WebVTT).
// Subtitles lie between the video frames of the file, so a window costs the video data of its
// interval and the whole track costs the whole file. The track is therefore loaded in portions
// while the video plays:
//   - the first window is built around the current position and is short, so the subtitles
//     appear quickly even when the torrent is still downloading;
//   - when less than half of the reserve ahead is left, the next window is requested;
//   - after a seek to a place that is not loaded, a window around the new position is requested;
//   - a window that was not read in time is followed by a shorter one.
// The loaded lines are collected into one file (the header of the first answer + the unique
// lines) that is handed over completely: onFirst the first time, onUpdate after each window.
// Lines that fall into several windows (long signs) are dropped by their text.

const DEFAULTS = {
  first: 30, // window around the position (start and seek), s
  chunk: 60, // window loaded ahead, s
  ahead: 90, // wanted reserve ahead of the position, s (the next window is requested below half of it)
  behind: 5, // how far back from the position a window around it starts, s
  min: 10, // windows do not get shorter than this after failures, s
  interval: 1000, // how often the position is checked, ms
  retries: 3,
  timeout: 130000, // longer than the server limit of a window: the server finishes the request itself
}

const log = (...args) => {
  // eslint-disable-next-line no-console
  console.info('[ASS]', 'embedded:', ...args)
}

// Splits an ASS answer into the header (up to the Format line of [Events]) and the event lines
export const splitAss = body => {
  const lines = String(body)
    .replace(/^\uFEFF/, '')
    .split(/\r\n|\n|\r/)
  const header = []
  const events = []
  let state = 0 // 0: before [Events], 1: waiting for Format:, 2: events
  lines.forEach(line => {
    if (state === 2) {
      if (/^(Dialogue|Comment):/i.test(line)) events.push(line)
      return
    }
    header.push(line)
    if (state === 0 && /^\[events\]\s*$/i.test(line.trim())) state = 1
    else if (state === 1 && /^format:/i.test(line.trim())) state = 2
  })
  return { header: state === 2 ? `${header.join('\n')}\n` : '', events }
}

// The same for WebVTT: the WEBVTT header and the cue blocks (everything between empty lines that has "-->")
export const splitVtt = body => {
  const text = String(body)
    .replace(/^\uFEFF/, '')
    .replace(/\r\n|\r/g, '\n')
  if (!/^WEBVTT/.test(text)) return { header: '', events: [] }
  const blocks = text.split(/\n{2,}/)
  blocks.shift() // the WEBVTT header (and its metadata)
  return {
    header: 'WEBVTT\n\n',
    events: blocks.map(block => block.trim()).filter(block => block.includes('-->')),
  }
}

const parseVttTime = value => {
  const match = /^(?:(\d+):)?(\d{2}):(\d{2})\.(\d{3})$/.exec(String(value).trim())
  return match ? Number(match[1] || 0) * 3600 + Number(match[2]) * 60 + Number(match[3]) + Number(match[4]) / 1000 : NaN
}

// WebVTT text -> [{ key, start, end, text }]; key identifies a cue (its whole block)
export const parseVttCues = text =>
  splitVtt(text)
    .events.map(block => {
      const lines = block.split('\n')
      const at = lines.findIndex(line => line.includes('-->'))
      const [from, to = ''] = lines[at].split('-->')
      const start = parseVttTime(from)
      const end = parseVttTime(to.trim().split(/\s+/)[0])
      if (Number.isNaN(start) || Number.isNaN(end)) return null
      return { key: block, start, end, text: lines.slice(at + 1).join('\n') }
    })
    .filter(Boolean)

const withParams = (url, params) => `${url}${url.includes('?') ? '&' : '?'}${params}`

export default class EmbeddedSubs {
  // options: { url, format: 'ass' | 'vtt', video, onFirst(text), onUpdate(text), onError(reason), options }
  constructor({ url, format = 'ass', video, onFirst, onUpdate, onError, options = {} }) {
    this.cfg = { ...DEFAULTS, ...options }
    this.url = url
    this.vtt = format === 'vtt'
    this.video = video
    this.onFirst = onFirst
    this.onUpdate = onUpdate
    this.onError = onError

    this.loaded = [] // loaded windows [from, to], sorted and merged
    this.seen = new Set()
    this.lines = []
    this.header = ''
    this.controller = null
    this.inflight = null
    this.seq = 0
    this.retries = 0
    this.timer = 0
    this.wait = 0
    this.given = false
    this.destroyed = false
    this.failed = false
    this.onSeek = () => this.tick()
  }

  start() {
    this.video.addEventListener('seeking', this.onSeek)
    this.tick()
    this.timer = window.setInterval(() => this.tick(), this.cfg.interval)
  }

  ranges() {
    return this.loaded.map(range => range.slice())
  }

  text() {
    if (this.vtt) return this.header + this.lines.join('\n\n') + (this.lines.length ? '\n\n' : '')
    return this.header + this.lines.join('\n') + (this.lines.length ? '\n' : '')
  }

  covered(t) {
    return this.loaded.find(range => t >= range[0] && t < range[1]) || null
  }

  addRange(from, to) {
    this.loaded.push([from, to])
    this.loaded.sort((a, b) => a[0] - b[0])
    const merged = []
    this.loaded.forEach(range => {
      const last = merged[merged.length - 1]
      if (last && range[0] <= last[1]) last[1] = Math.max(last[1], range[1])
      else merged.push([range[0], range[1]])
    })
    this.loaded = merged
  }

  accept(body, from, to) {
    const parts = this.vtt ? splitVtt(body) : splitAss(body)
    if (!this.header) this.header = parts.header
    // no header: the answer does not look like ASS/WebVTT
    if (!this.header) return false
    parts.events.forEach(line => {
      if (this.seen.has(line)) return
      this.seen.add(line)
      this.lines.push(line)
    })
    this.addRange(from, to)
    return true
  }

  async request(from, to) {
    if (this.controller) this.controller.abort()
    this.seq += 1
    const id = this.seq
    const start = Math.max(0, Math.floor(from))
    const end = Math.ceil(to)
    const controller = new AbortController()
    this.controller = controller
    this.inflight = { id, from: start, to: end }
    const timeout = window.setTimeout(() => controller.abort(), this.cfg.timeout)
    const started = Date.now()
    log(`window ${start}-${end} s requested`)

    try {
      const response = await fetch(withParams(this.url, `from=${start}&to=${end}`), { signal: controller.signal })
      if (this.destroyed || id !== this.seq) return
      if (!response.ok) throw new Error(`HTTP ${response.status}`)
      const body = await response.text()
      if (this.destroyed || id !== this.seq) return
      this.inflight = null
      const before = this.lines.length
      if (!this.accept(body, start, end)) {
        this.fail(`answer is not an ${this.vtt ? 'WebVTT' : 'ASS'} file (${body.slice(0, 80).replace(/\s+/g, ' ')})`)
        return
      }
      this.retries = 0
      log(
        `window ${start}-${end} s loaded in ${Date.now() - started} ms, +${this.lines.length - before} lines, ` +
          `total ${this.lines.length}, loaded ${JSON.stringify(this.loaded)}`,
      )
      if (!this.given) {
        this.given = true
        this.onFirst(this.text())
      } else {
        this.onUpdate(this.text())
      }
      this.tick()
    } catch (error) {
      if (this.destroyed || id !== this.seq) return
      this.inflight = null
      this.fail(`window ${start}-${end} s failed after ${Date.now() - started} ms (${error.message})`)
    } finally {
      window.clearTimeout(timeout)
    }
  }

  fail(reason) {
    this.retries += 1
    // the window was not read in time: the next attempt is twice as short
    this.cfg.first = Math.max(this.cfg.min, Math.floor(this.cfg.first / 2))
    this.cfg.chunk = Math.max(this.cfg.min, Math.floor(this.cfg.chunk / 2))
    log(`${reason} (attempt ${this.retries} of ${this.cfg.retries})`)
    if (this.retries > this.cfg.retries) {
      this.failed = true
      if (this.onError) this.onError(reason)
      return
    }
    this.wait = window.setTimeout(() => {
      this.wait = 0
      this.tick()
    }, 3000 * this.retries)
  }

  tick() {
    if (this.destroyed || this.failed || this.wait) return
    const t = Math.max(0, this.video.currentTime || 0)
    const range = this.covered(t)

    if (this.inflight) {
      // a seek: the request in progress is not about this place any more
      if (!range && (t < this.inflight.from || t >= this.inflight.to)) this.request(t - this.cfg.behind, t + this.cfg.first)
      return
    }
    if (!range) {
      this.request(t - this.cfg.behind, t + this.cfg.first)
      return
    }
    const duration = Number.isFinite(this.video.duration) ? this.video.duration : 0
    if (range[1] - t < this.cfg.ahead / 2 && (!duration || range[1] < duration)) this.request(range[1], range[1] + this.cfg.chunk)
  }

  destroy() {
    this.destroyed = true
    window.clearInterval(this.timer)
    window.clearTimeout(this.wait)
    if (this.controller) this.controller.abort()
    this.controller = null
    this.video.removeEventListener('seeking', this.onSeek)
  }
}
