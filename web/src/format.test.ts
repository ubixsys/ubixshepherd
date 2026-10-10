import { age, since, duration, usd, runCost, clockTime, shortSha, tokens } from './format'

// Test age function
describe('age', () => {
  it('should format seconds correctly', () => {
    expect(age(30000)).toBe('30s')
    expect(age(59000)).toBe('59s')
    expect(age(60000)).toBe('1m')
  })

  it('should format minutes correctly', () => {
    expect(age(60000)).toBe('1m')
    expect(age(3599000)).toBe('59m')
    expect(age(3600000)).toBe('1h')
  })

  it('should format hours correctly', () => {
    expect(age(3600000)).toBe('1h')
    expect(age(47 * 3600000)).toBe('47h')
    expect(age(48 * 3600000)).toBe('2d')
  })

  it('should format days correctly', () => {
    expect(age(48 * 3600000)).toBe('2d')
    expect(age(100 * 3600000)).toBe('4d')
  })

  it('should handle negative values', () => {
    expect(age(-1)).toBe('0s')
    expect(age(-1000000)).toBe('0s')
  })
})

// Test since function
describe('since', () => {
  const now = Date.parse('2026-10-08T14:00:00')

  it('should handle empty or undefined input', () => {
    expect(since(undefined, now)).toBe('')
    expect(since('', now)).toBe('')
  })

  it('should handle unparsable time', () => {
    expect(since('invalid', now)).toBe('')
  })

  it('should handle future times', () => {
    const future = new Date(now + 3600000).toISOString()
    expect(since(future, now)).toBe('')
  })

  it('should calculate time differences correctly', () => {
    // 3 minutes ago
    const threeMinutesAgo = new Date(now - 3 * 60_000).toISOString()
    expect(since(threeMinutesAgo, now)).toBe('3m')
    
    // 1 hour ago
    const oneHourAgo = new Date(now - 3600000).toISOString()
    expect(since(oneHourAgo, now)).toBe('1h')
    
    // 2 days ago
    const twoDaysAgo = new Date(now - 2 * 24 * 3600000).toISOString()
    expect(since(twoDaysAgo, now)).toBe('2d')
  })
})

// Test duration function
describe('duration', () => {
  const now = Date.parse('2026-10-08T14:00:00')
  
  it('should handle unparsable start time', () => {
    expect(duration('invalid', undefined, now)).toBe('')
  })

  it('should format seconds correctly', () => {
    const start = '2026-10-08T13:59:30'
    const end = '2026-10-08T13:59:50'
    expect(duration(start, end, now)).toBe('20s')
  })

  it('should format minutes correctly', () => {
    const start = '2026-10-08T13:00:00'
    const end = '2026-10-08T13:15:30'
    expect(duration(start, end, now)).toBe('15m 30s')
  })

  it('should format hours correctly with zero-padded minutes', () => {
    const start = '2026-10-08T10:00:00'
    const end = '2026-10-08T13:05:00'
    expect(duration(start, end, now)).toBe('3h 05m')
  })

  it('should handle durations across days', () => {
    const start = '2026-10-07T22:00:00'
    const end = '2026-10-08T02:30:00'
    expect(duration(start, end, now)).toBe('4h 30m')
  })
})

// Test usd function
describe('usd', () => {
  it('should format zero correctly', () => {
    expect(usd(0)).toBe('$0.00')
  })

  it('should format amounts under a cent', () => {
    expect(usd(0.001)).toBe('<$0.01')
    expect(usd(0.005)).toBe('<$0.01')
  })

  it('should format amounts less than $100 with 2 decimals', () => {
    expect(usd(1.234)).toBe('$1.23')
    expect(usd(12.5)).toBe('$12.50')
    expect(usd(99.99)).toBe('$99.99')
  })

  it('should format amounts $100 and up as whole dollars', () => {
    expect(usd(100)).toBe('$100')
    expect(usd(123.45)).toBe('$123')
    expect(usd(1000)).toBe('$1000')
  })

  it('should handle negative amounts', () => {
    expect(usd(-1)).toBe('')
  })
})

// Test runCost function
describe('runCost', () => {
  it('should handle empty run', () => {
    expect(runCost({})).toBe('')
  })

  it('should format USD costs correctly', () => {
    expect(runCost({ cost_usd: 1.23 })).toBe('$1.23')
    expect(runCost({ cost_usd: 0.005 })).toBe('<$0.01')
  })

  it('should format credits correctly', () => {
    expect(runCost({ credits: 3 })).toBe('3 credits')
    expect(runCost({ credits: 3.5 })).toBe('3.5 credits')
    expect(runCost({ credits: 1 })).toBe('1 credit')
    expect(runCost({ credits: 2.96 })).toBe('3 credits')
  })

  it('should combine USD and credits', () => {
    expect(runCost({ cost_usd: 1.23, credits: 3 })).toBe('$1.23 + 3 credits')
    expect(runCost({ cost_usd: 1.23, credits: 3.5 })).toBe('$1.23 + 3.5 credits')
  })

  it('should handle zero values correctly', () => {
    expect(runCost({ cost_usd: 0, credits: 0 })).toBe('')
    expect(runCost({ cost_usd: 0, credits: 3 })).toBe('3 credits')
    expect(runCost({ cost_usd: 1.23, credits: 0 })).toBe('$1.23')
  })
})

// Test clockTime function
describe('clockTime', () => {
  const now = Date.parse('2026-10-08T14:00:00')

  it('should handle unparsable time', () => {
    expect(clockTime('invalid', now)).toBe('')
  })

  it('should show only time for same day', () => {
    const today = new Date(now).toISOString()
    expect(clockTime(today, now)).toBe('14:00')
  })

  it('should write midnight as 00, not 24', () => {
    const midnight = new Date(Date.parse('2026-10-08T00:05:00')).toISOString()
    expect(clockTime(midnight, now)).toBe('00:05')
  })

  it('should show date and time for different days', () => {
    const yesterday = new Date(now - 24 * 3600000).toISOString()
    expect(clockTime(yesterday, now)).toBe('Oct 7 14:00')
    
    // With 30 days difference (Sept 8, 2026)
    const lastMonth = new Date(now - 30 * 24 * 3600000).toISOString()
    expect(clockTime(lastMonth, now)).toBe('Sep 8 14:00')
  })
})

// Test shortSha function
describe('shortSha', () => {
  it('should return empty string for undefined', () => {
    expect(shortSha(undefined)).toBe('')
  })

  it('should return first 8 characters of SHA', () => {
    expect(shortSha('1234567890abcdef')).toBe('12345678')
    expect(shortSha('1234567')).toBe('1234567')
  })
})
describe('tokens', () => {
  it('abbreviates counts', () => {
    expect(tokens(0)).toBe('0')
    expect(tokens(850)).toBe('850')
    expect(tokens(12_300)).toBe('12.3k')
    expect(tokens(412_000)).toBe('412k')
    expect(tokens(200_000)).toBe('200k')
    expect(tokens(1_800_000)).toBe('1.8M')
    expect(tokens(18_400_000)).toBe('18M')
  })
})
