import { getTorrServerHost, subsHost } from './Hosts'

// Link to the list of external subtitle tracks of a video file: GET /subs/{hash}/{id}
export const subsListUrl = (hash, fileId) => `${subsHost()}/${encodeURIComponent(hash)}/${fileId}`

// Resolves to [] on any error: subtitles are optional (old servers have no /subs)
export const fetchExternalSubtitles = async (url, signal) => {
  try {
    const response = await fetch(url, { signal })
    if (!response.ok) return []
    const list = await response.json()
    return Array.isArray(list) ? list : []
  } catch {
    return []
  }
}

export const isAssTrack = track => track.format === 'ass' || track.format === 'ssa'

// Absolute link to the subtitle file; original=true returns the file as is (needed for ASS)
export const subtitleFileUrl = (track, original = false) => {
  const url = new URL(track.url, `${getTorrServerHost()}/`)
  if (original) url.searchParams.set('format', 'original')
  return url.toString()
}

// File index (the "index" query parameter) of a stream link such as the one used for captions
export const linkFileIndex = link => {
  const match = /[?&]index=(\d+)/.exec(link || '')
  return match ? Number(match[1]) : null
}

// Name of an external track: its title, or the file name
export const trackLabel = track => track.title || track.name || 'Subtitle'

// Link to the fonts for the ASS subtitles of a video file: the font files of the torrent and the fonts attached
// to the video file, in one list (GET /subs/{hash}/{id}?fonts)
export const fontsListUrl = subsLink => `${subsLink}?fonts`

// Resolves to [{ path, url, source }], source is 'external' (a file of the torrent) or 'embedded' (attached to
// the video file). [] on any error. An old server answers ?fonts with the list of subtitles: it has no 'source'.
export const fetchFontFiles = async (url, signal) => {
  try {
    const response = await fetch(url, { signal })
    if (!response.ok) return []
    const list = await response.json()
    if (!Array.isArray(list)) return []
    return list
      .filter(item => item && typeof item.source === 'string' && item.name && item.url)
      .map(item => ({
        path: item.name,
        url: new URL(item.url, `${getTorrServerHost()}/`).toString(),
        source: item.source,
      }))
  } catch {
    return []
  }
}
