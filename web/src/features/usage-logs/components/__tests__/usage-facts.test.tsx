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
import { act, render, screen, within } from '@testing-library/react'
import i18next, { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeAll, describe, expect, test } from 'vitest'

import type { UsageLog } from '../../data/schema'
import type { LogOtherData } from '../../types'
import { DetailsDialog } from '../dialogs/details-dialog'

const i18nKeys = {
  'Log Details': 'Log Details',
  Consume: 'Consume',
  'Billing Details': 'Billing Details',
  'Billing Mode': 'Billing Mode',
  'Per-token': 'Per-token',
  'Dynamic Pricing': 'Dynamic Pricing',
  'Matched Tier': 'Matched Tier',
  'Group Ratio': 'Group Ratio',
  'Total Cost': 'Total Cost',
  'Usage parameters': 'Usage parameters',
}

function makeLog(other: LogOtherData): UsageLog {
  return {
    id: 1,
    user_id: 1,
    created_at: 1,
    type: 2,
    content: '',
    username: 'user',
    token_name: 'token',
    model_name: 'wan2.5-i2v-preview',
    quota: 5000,
    prompt_tokens: 0,
    completion_tokens: 0,
    use_time: 0,
    is_stream: false,
    channel: 1,
    channel_name: '',
    token_id: 1,
    group: 'default',
    ip: '',
    other: JSON.stringify(other),
    request_id: 'req-1',
    upstream_request_id: '',
  }
}

function renderDetails(
  other: LogOtherData,
  promptTokens = 0,
  translations = i18next
): QueryClient {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const freshAt = Date.now() + 60_000
  queryClient.setQueryData(['status'], {}, { updatedAt: freshAt })
  queryClient.setQueryData(
    ['pricing'],
    { data: [], vendors: [] },
    { updatedAt: freshAt }
  )

  render(
    <I18nextProvider i18n={translations}>
      <QueryClientProvider client={queryClient}>
        <DetailsDialog
          log={{ ...makeLog(other), prompt_tokens: promptTokens }}
          isAdmin={false}
          isRoot={false}
          open
          onOpenChange={() => undefined}
        />
      </QueryClientProvider>
    </I18nextProvider>
  )
  return queryClient
}

function rowValue(label: string): string | null {
  return screen.getByText(label).nextElementSibling?.textContent ?? null
}

const storageExpression =
  'tier("standard", p * 1.25 + c * 10 + cr * 0.125 + cs * 4.5)'
const cancelledStorage =
  'Explicit cache creation was cancelled because the storage charge exceeds the available quota; only the cached input tokens were charged'

function explicitCacheCharge(
  action: 'create' | 'extend',
  seconds: number,
  storageTokenHours: number
): LogOtherData {
  return {
    group_ratio: 1,
    billing_mode: 'tiered_expr',
    expr_b64: btoa(storageExpression),
    matched_tier: 'standard',
    billing_tokens: {
      p: action === 'create' ? 12000 : 0,
      c: 0,
      len: 12000,
      cr: 0,
      cs: storageTokenHours,
    },
    explicit_cache: {
      action,
      name: 'cachedContents/abc123',
      tokens: 12000,
      seconds,
      storage_token_hours: storageTokenHours,
    },
    content_parts: [
      {
        key:
          action === 'create'
            ? 'Explicit cache created: {{tokens}} tokens stored for {{seconds}} seconds'
            : 'Explicit cache extended: {{tokens}} tokens stored for {{seconds}} more seconds',
        params: { tokens: 12000, seconds },
      },
    ],
  }
}

describe('explicit cache charges', () => {
  const queryClients: QueryClient[] = []
  afterEach(() => {
    for (const queryClient of queryClients) queryClient.clear()
    queryClients.length = 0
  })

  test.each([
    {
      action: 'create' as const,
      seconds: 3600,
      hours: 12000,
      label: 'Create',
      duration: '1 hour (3,600 seconds)',
      storage: '12,000 token·hours',
      content: 'Explicit cache created: 12000 tokens stored for 3600 seconds',
    },
    {
      action: 'extend' as const,
      seconds: 1800,
      hours: 6000,
      label: 'Extend',
      duration: '30 minutes (1,800 seconds)',
      storage: '6,000 token·hours',
      content:
        'Explicit cache extended: 12000 tokens stored for 1800 more seconds',
    },
  ])(
    'a $action charge shows its cache, storage time and token-hours with the storage price',
    ({ action, seconds, hours, label, duration, storage, content }) => {
      queryClients.push(
        renderDetails(
          explicitCacheCharge(action, seconds, hours),
          action === 'create' ? 12000 : 0
        )
      )
      expect(rowValue('Action')).toBe(label)
      expect(rowValue('Cache name')).toBe('cachedContents/abc123')
      expect(rowValue('Cached tokens')).toBe('12,000')
      expect(rowValue('Storage duration')).toBe(duration)
      expect(rowValue('Storage')).toBe(storage)
      expect(screen.getByText(content)).toBeVisible()
      expect(
        screen
          .getAllByText('Cache Storage')
          .map((element) => element.nextElementSibling?.textContent)
      ).toContain('$4.5/1M tokens / hour')
      expect(
        screen.getAllByText('Cache Storage / 1M tokens / hour').length
      ).toBeGreaterThan(0)
      expect(screen.queryByText(cancelledStorage)).not.toBeInTheDocument()
    }
  )

  test('the billable token breakdown lists storage in token-hours', () => {
    queryClients.push(
      renderDetails(explicitCacheCharge('create', 3600, 12000), 12000)
    )
    const billable = within(
      screen.getByRole('group', { name: 'Billable token breakdown' })
    )
    expect(
      billable.getByText('Cache Storage').nextElementSibling
    ).toHaveTextContent('12,000 token·hours')
    expect(billable.getByText('Input').nextElementSibling).toHaveTextContent(
      '12,000'
    )
  })

  test('a creation whose storage was cancelled explains that only input was charged', () => {
    queryClients.push(
      renderDetails(
        {
          ...explicitCacheCharge('create', 0, 0),
          billing_tokens: { p: 12000, c: 0, len: 12000, cr: 0 },
          explicit_cache: {
            action: 'create',
            name: 'cachedContents/abc123',
            tokens: 12000,
            seconds: 0,
            storage_token_hours: 0,
            storage_cancelled: true,
          },
          content_parts: [{ key: cancelledStorage }],
        },
        12000
      )
    )
    const section = screen.getByText('Explicit cache').parentElement
    if (!section) throw new Error('Expected the explicit cache section')
    expect(within(section).getByText(cancelledStorage)).toBeVisible()
    expect(rowValue('Storage duration')).toBe('0 seconds')
    expect(rowValue('Storage')).toBe('0 token·hours')
    expect(screen.queryByText('Cache Storage')).not.toBeInTheDocument()
    expect(
      screen.queryByText('Cache Storage / 1M tokens / hour')
    ).not.toBeInTheDocument()
  })

  test('an ordinary request on a storage-priced model omits storage details', () => {
    queryClients.push(
      renderDetails(
        {
          group_ratio: 1,
          billing_mode: 'tiered_expr',
          expr_b64: btoa(storageExpression),
          matched_tier: 'standard',
          cache_tokens: 100,
          billing_tokens: { p: 900, c: 10, len: 1000, cr: 100 },
        },
        1000
      )
    )
    expect(screen.queryByText('Explicit cache')).not.toBeInTheDocument()
    expect(screen.queryByText('Cache Storage')).not.toBeInTheDocument()
    expect(
      screen.queryByText('Cache Storage / 1M tokens / hour')
    ).not.toBeInTheDocument()
    expect(
      screen
        .getAllByText('Cache Read')
        .map((element) => element.nextElementSibling?.textContent)
    ).toContain('$0.125/M')
  })

  test('reformats cached tokens and storage time after a language switch', async () => {
    const translations = createInstance()
    await translations.init({
      lng: 'en',
      fallbackLng: 'en',
      resources: { en: { translation: {} }, fr: { translation: {} } },
    })
    queryClients.push(
      renderDetails(
        explicitCacheCharge('create', 3600, 12000),
        12000,
        translations
      )
    )
    expect(rowValue('Cached tokens')).toBe('12,000')
    expect(rowValue('Storage duration')).toBe('1 hour (3,600 seconds)')
    await act(() => translations.changeLanguage('fr'))
    // French groups digits and separates units with no-break spaces.
    expect(rowValue('Cached tokens')).toMatch(/^12\s000$/u)
    expect(rowValue('Storage duration')).toMatch(
      /^1\sheure\s\(3\s600\ssecondes\)$/u
    )
  })
})

test('shows the recorded request and response models in log details', () => {
  const queryClient = renderDetails({
    response_model: {
      requested_model: 'requested-model',
      upstream_model: 'mapped-model',
      returned_model: 'unexpected-model',
    },
  })
  expect(screen.getByText('Response model: unexpected-model')).toBeVisible()
  expect(rowValue('Request Model')).toBe('requested-model')
  expect(rowValue('Upstream Model')).toBe('mapped-model')
  expect(screen.getByText('unexpected-model')).toBeVisible()
  queryClient.clear()
})

describe('usage facts billing details', () => {
  test('shows the settled image count and a per-image price', () => {
    const queryClient = renderDetails({
      billing_mode: 'tiered_expr',
      expr_b64: btoa('tier("image", fixed(0.04)) * image_count'),
      billing_unit: 'request',
      fixed_price: 0.04,
      image_count: 2,
      matched_tier: 'image',
    })
    expect(
      screen.getByText('Billable image count').parentElement
    ).toHaveTextContent('2')
    expect(screen.getAllByText(/\/image/).length).toBeGreaterThan(0)
    queryClient.clear()
  })
  const queryClients: QueryClient[] = []

  test('shows actual billable image and cache tokens while retaining the aggregate cache count', () => {
    queryClients.push(
      renderDetails(
        {
          billing_mode: 'tiered_expr',
          expr_b64: btoa(
            'tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 2 + c * 30)'
          ),
          matched_tier: 'standard',
          cache_tokens: 300,
          image_cache_tokens: 200,
          billing_tokens: { p: 300, cr: 100, img: 400, img_cr: 200, c: 100 },
        },
        1000
      )
    )
    const billable = within(
      screen.getByRole('group', { name: 'Billable token breakdown' })
    )
    expect(
      billable.getByText('Image Cache').nextElementSibling
    ).toHaveTextContent('200')
    expect(
      billable.getByText('Cache Read').nextElementSibling
    ).toHaveTextContent('100')
    expect(billable.getByText('Image In').nextElementSibling).toHaveTextContent(
      '400'
    )
    expect(
      screen.getByText('Input Tokens').nextElementSibling
    ).toHaveTextContent('1,000')
    expect(
      screen
        .getAllByText('Cache Read')
        .some((label) => label.nextElementSibling?.textContent === '300')
    ).toBe(true)
  })

  beforeAll(() => {
    i18next.addResourceBundle('en', 'translation', i18nKeys)
  })

  afterEach(() => {
    for (const queryClient of queryClients) {
      queryClient.clear()
    }
    queryClients.length = 0
  })

  test('renders one raw-key row per usage fact before total cost', () => {
    const expression = 'tier("720P", u("seconds") * 5)'
    queryClients.push(
      renderDetails({
        group_ratio: 1,
        billing_mode: 'tiered_expr',
        expr_b64: Buffer.from(expression, 'utf8').toString('base64'),
        matched_tier: '720P',
        usage_facts: {
          resolution: '720P',
          seconds: 5,
        },
      })
    )

    expect(screen.getByText('Usage parameters')).toBeInTheDocument()
    expect(rowValue('resolution')).toBe('720P')
    expect(rowValue('seconds')).toBe('5')
    expect(rowValue('Billing Mode')).toBe('Dynamic Pricing')
    expect(rowValue('Matched Tier')).toBe('720P')

    const usageHeader = screen.getByText('Usage parameters')
    const totalCost = screen.getByText('Total Cost')
    expect(
      usageHeader.compareDocumentPosition(totalCost) &
        Node.DOCUMENT_POSITION_FOLLOWING
    ).toBeTruthy()
  })

  test('does not render usage parameter rows when usage_facts is absent', () => {
    queryClients.push(
      renderDetails({
        group_ratio: 1,
      })
    )

    expect(screen.queryByText('Usage parameters')).toBeNull()
    expect(screen.queryByText('resolution')).toBeNull()
    expect(screen.queryByText('seconds')).toBeNull()
    expect(screen.getByText('Total Cost')).toBeInTheDocument()
  })

  test('does not render usage parameter rows when usage_facts is empty', () => {
    queryClients.push(
      renderDetails({
        group_ratio: 1,
        usage_facts: {},
      })
    )

    expect(screen.queryByText('Usage parameters')).toBeNull()
    expect(screen.queryByText('resolution')).toBeNull()
    expect(screen.queryByText('seconds')).toBeNull()
    expect(screen.getByText('Total Cost')).toBeInTheDocument()
  })
})
