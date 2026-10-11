/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { GeminiSettingsCard } from '../gemini-settings-card'

function Fixture() {
  const [container, setContainer] = useState<HTMLDivElement | null>(null)
  return (
    <>
      <div ref={setContainer} />
      <SettingsPageProvider actionsContainer={container}>
        <GeminiSettingsCard
          defaultValues={{
            gemini: {
              safety_settings: '',
              version_settings: '',
              supported_imagine_models: '',
              thinking_adapter_enabled: false,
              thinking_adapter_budget_tokens_percentage: 0.6,
              function_call_thought_signature_enabled: true,
              remove_function_response_id_enabled: true,
              explicit_cache_max_ttl_seconds: 86400,
            },
          }}
        />
      </SettingsPageProvider>
    </>
  )
}

let client: QueryClient

beforeEach(() => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  vi.spyOn(api, 'put').mockResolvedValue({ data: { success: true } })
})

afterEach(() => {
  client.clear()
})

async function renderSettings() {
  render(
    <QueryClientProvider client={client}>
      <Fixture />
    </QueryClientProvider>
  )
  return screen.findByRole('spinbutton', {
    name: 'Explicit cache max TTL (seconds)',
  })
}

test('saves only a changed explicit cache max TTL starting from the 24-hour default', async () => {
  const input = await renderSettings()
  expect(input).toHaveValue(86400)
  fireEvent.change(input, { target: { value: '3600' } })
  fireEvent.click(screen.getByRole('button', { name: 'Save Changes' }))
  await waitFor(() =>
    expect(api.put).toHaveBeenCalledWith('/api/option/', {
      key: 'gemini.explicit_cache_max_ttl_seconds',
      value: 3600,
    })
  )
  expect(api.put).toHaveBeenCalledTimes(1)
})

test.each(['0', '-1', '1.5'])(
  'rejects an explicit cache max TTL of %s with a field error',
  async (value) => {
    const input = await renderSettings()
    fireEvent.change(input, { target: { value } })
    fireEvent.click(screen.getByRole('button', { name: 'Save Changes' }))
    await waitFor(() => expect(input).toHaveAttribute('aria-invalid', 'true'))
    expect(screen.getByText('Enter a positive integer')).toBeVisible()
    expect(api.put).not.toHaveBeenCalled()
  }
)
