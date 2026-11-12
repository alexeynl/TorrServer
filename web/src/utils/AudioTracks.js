import axios from 'axios'

import { gstreamerProbeUrl } from './GStreamer'

const probeTrackValue = (track, name) => track?.[name] ?? track?.[`${name[0].toLowerCase()}${name.slice(1)}`]

const probeAudioTracks = probe =>
  (probe?.Tracks || probe?.tracks || []).filter(
    track => String(probeTrackValue(track, 'Type')).toLowerCase() === 'audio',
  )

const audioCodecName = track => {
  const codec = String(probeTrackValue(track, 'Codec') || '')
  const caps = String(probeTrackValue(track, 'CapsName') || '')
  const value = `${caps} ${codec}`.toLowerCase()

  switch (true) {
    case value.includes('eac3') || value.includes('e-ac3') || value.includes('e-ac-3'):
      return 'E-AC3'
    case value.includes('truehd') || value.includes('true-hd') || value.includes('mlp'):
      return 'TrueHD'
    case value.includes('ac3') || value.includes('ac-3') || value.includes('a/52'):
      return 'AC3'
    case value.includes('dts'):
      return 'DTS'
    case value.includes('aac') || value.includes('mpegversion=(int)4') || value.includes('mpegversion=4'):
      return 'AAC'
    case value.includes('opus'):
      return 'Opus'
    case value.includes('vorbis'):
      return 'Vorbis'
    case value.includes('flac'):
      return 'FLAC'
    case value.includes('mp3') || value.includes('layer=(int)3'):
      return 'MP3'
    case value.includes('pcm') || value.includes('audio/x-raw'):
      return 'PCM'
    default: {
      const shortName = (caps || codec).split(',')[0].trim()
      return shortName
        .replace(/^audio\/(?:x-)?/i, '')
        .replace(/[_-]+/g, ' ')
        .toUpperCase()
    }
  }
}

const audioTrackLabel = (track, ordinal, t) => {
  const trackTitle = String(probeTrackValue(track, 'Title') || '').trim()
  const language = String(probeTrackValue(track, 'Language') || '').trim()
  const codec = audioCodecName(track)
  const channels = Number(probeTrackValue(track, 'Channels'))
  const rate = Number(probeTrackValue(track, 'Rate'))
  const details = [
    trackTitle && language ? language.toUpperCase() : '',
    codec,
    channels > 0 ? `${channels} ch` : '',
    rate > 0 ? `${Math.round(rate / 1000)} kHz` : '',
  ].filter(Boolean)
  return {
    primary: trackTitle || language.toUpperCase() || `${t('GStreamer.AudioTrack')} ${ordinal + 1}`,
    secondary: details.join(' / '),
  }
}

// One entry per selectable track; audio is the value of the "audio" parameter of master.m3u8.
export const buildAudioOptions = (probe, t) =>
  probeAudioTracks(probe).map((track, ordinal) => {
    const value = Number(probeTrackValue(track, 'Index'))
    const audio = Number.isInteger(value) && value >= 0 ? value : ordinal
    return { key: `embedded:${audio}`, audio, external: false, ...audioTrackLabel(track, ordinal, t) }
  })

// Rejects when the probe fails.
export const loadAudioOptions = async (hash, fileId, t) => {
  const { data: probe } = await axios.get(gstreamerProbeUrl(hash, fileId))
  return buildAudioOptions(probe, t)
}
