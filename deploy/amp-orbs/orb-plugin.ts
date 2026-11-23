// livy: the orb bootstrap for hosted Amp orbs, bundled by deploy/amp-orbs/build.sh as index.ts.
// Loaded as a personal Amp plugin, so it reaches every orb thread. Outside an orb it does nothing:
// the laptop's own livy.ts records local threads. In an orb it puts the bundled livy binary and
// config in place, then hands Amp to recorder.ts, the plugin `livy install` writes on a laptop,
// rendered for the orb's paths. Uploads go to the git sink named in config.json, with the git
// credentials Amp gives the orb; nothing here holds a credential.
import type { PluginAPI } from '@ampcode/plugin'
import { createHash } from 'node:crypto'
import { chmodSync, existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { gunzipSync } from 'node:zlib'
import record from './recorder.ts'

// The orb user's home, as recorder.ts was rendered for it.
const HOME = '/home/user'
const DATA = `${HOME}/.livy`
const BIN = `${DATA}/bin/livy`
const HERE = fileURLToPath(new URL('.', import.meta.url))

const sha = (b: Uint8Array) => createHash('sha256').update(b).digest('hex')

// place writes body to path atomically, unless the stamp says these bytes are already there.
function place(path: string, body: () => Uint8Array, stamp: string, mode: number): void {
  const stampFile = `${path}.sha256`
  try {
    if (existsSync(path) && readFileSync(stampFile, 'utf8') === stamp) return
  } catch {}
  writeFileSync(`${path}.tmp`, body(), { mode })
  chmodSync(`${path}.tmp`, mode)
  renameSync(`${path}.tmp`, path)
  writeFileSync(stampFile, stamp, { mode: 0o600 })
}

function prepare(): void {
  const arch = process.arch === 'arm64' ? 'arm64' : 'amd64'
  const gz = readFileSync(join(HERE, `livy-linux-${arch}.gz`))
  const config = readFileSync(join(HERE, 'config.json'))
  mkdirSync(`${DATA}/bin`, { recursive: true, mode: 0o700 })
  place(BIN, () => gunzipSync(gz), sha(gz), 0o755)
  place(`${DATA}/config.json`, () => config, sha(config), 0o600)
}

export default function (amp: PluginAPI) {
  const inOrb = process.env.AMP_ORB === '1' || amp.system.executor?.kind === 'remote'
  if (!inOrb) return
  if (homedir() !== HOME) {
    console.error(`livy: this orb's home is not ${HOME}; Amp threads here are not recorded`)
    return
  }
  try {
    prepare()
  } catch (err) {
    console.error('livy: could not put the recorder in place; Amp threads here are not recorded:', err)
    return
  }
  record(amp)
}
