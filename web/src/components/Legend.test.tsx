import { render, screen } from '@testing-library/react'
import { Legend } from './Legend'

describe('Legend', () => {
  it('renders one dd per event', () => {
    render(<Legend />)
    
    // There should be 23 events
    const dds = screen.getAllByRole('definition')
    expect(dds).toHaveLength(23)
  })
  
  it('says what each glyph means in its own words', () => {
    render(<Legend />)
    const meanings = screen.getAllByRole('definition').map((dd) => dd.textContent)
    expect(meanings).toContain('run passed')
    expect(meanings).toContain('request needs you')
  })
})
