import { describe, expect, it } from 'vitest'
import { refusal } from './daemon-proxy.ts'

const page = 'http://localhost:5178'

describe('the dev proxy refuses what the daemon would', () => {
  it('lets the dev page through', () => {
    expect(refusal('GET', { host: 'localhost:5178' })).toBeNull()
    expect(refusal('GET', { host: 'localhost:5178', 'sec-fetch-site': 'same-origin' })).toBeNull()
    expect(refusal('POST', { host: 'localhost:5178', origin: page, 'sec-fetch-site': 'same-origin' })).toBeNull()
    expect(refusal('GET', { host: '127.0.0.1:5178' })).toBeNull()
    expect(refusal('GET', { host: '[::1]:5178' })).toBeNull()
  })

  it('refuses a rebound host', () => {
    expect(refusal('GET', { host: 'evil.example:5178' })).toMatch(/not loopback/)
    expect(refusal('GET', { host: '127.0.0.1.evil.example:5178' })).toMatch(/not loopback/)
    expect(refusal('GET', {})).toMatch(/not loopback/)
  })

  it('refuses another origin or site', () => {
    expect(refusal('POST', { host: 'localhost:5178', origin: 'http://evil.example' })).toMatch(/evil/)
    expect(refusal('POST', { host: 'localhost:5178', origin: 'http://localhost:8080' })).not.toBeNull()
    expect(refusal('GET', { host: 'localhost:5178', 'sec-fetch-site': 'cross-site' })).not.toBeNull()
    expect(refusal('GET', { host: 'localhost:5178', 'sec-fetch-site': 'same-site' })).not.toBeNull()
  })

  it('refuses a write without Origin', () => {
    expect(refusal('POST', { host: 'localhost:5178' })).toMatch(/Origin/)
  })
})
