import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import UpstreamBalanceCell from '../UpstreamBalanceCell.vue'
import type { Account, UpstreamBalanceProbeSnapshot } from '@/types'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}:${Object.values(params).join(',')}` : key
    })
  }
})

const NOW = Date.parse('2026-05-20T10:00:00Z')

const makeAccount = (overrides: Partial<Account> = {}): Account => ({
  id: 1,
  name: 'relay',
  platform: 'openai',
  type: 'apikey',
  proxy_id: null,
  concurrency: 1,
  priority: 1,
  status: 'active',
  error_message: null,
  last_used_at: null,
  expires_at: null,
  auto_pause_on_expired: false,
  created_at: '2026-05-01T00:00:00Z',
  updated_at: '2026-05-01T00:00:00Z',
  schedulable: true,
  rate_limited_at: null,
  rate_limit_reset_at: null,
  overload_until: null,
  temp_unschedulable_until: null,
  temp_unschedulable_reason: null,
  session_window_start: null,
  session_window_end: null,
  session_window_status: null,
  ...overrides,
  extra: { ...overrides.extra }
})

const snapshot = (overrides: Partial<UpstreamBalanceProbeSnapshot> = {}): UpstreamBalanceProbeSnapshot => ({
  status: 'ok',
  data: {
    kind: 'sub2api',
    remaining_usd: 12.5,
    mode: 'quota_limited',
    observed_at: '2026-05-20T09:58:00Z'
  },
  received_at: '2026-05-20T09:58:00Z',
  fresh_until: '2026-05-20T10:58:00Z',
  last_attempt_at: '2026-05-20T09:58:00Z',
  next_probe_at: '2026-05-20T10:28:00Z',
  ...overrides
})

const mountCell = (account: Account, probing = false) =>
  mount(UpstreamBalanceCell, { props: { account, now: NOW, probing } })

describe('UpstreamBalanceCell', () => {
  it('renders nothing for non API-key accounts', () => {
    const wrapper = mountCell(makeAccount({ type: 'oauth' }))
    expect(wrapper.find('[data-testid="upstream-balance-cell"]').exists()).toBe(false)
  })

  it('shows not-probed state with a query button before the first probe', () => {
    const wrapper = mountCell(makeAccount())
    expect(wrapper.get('[data-testid="upstream-balance"]').text()).toContain('admin.accounts.upstreamBalance.notProbed')
    expect(wrapper.find('[data-testid="upstream-balance-probe"]').exists()).toBe(true)
  })

  it('formats the remaining credit in USD and exposes totals in the tooltip', async () => {
    const wrapper = mount(UpstreamBalanceCell, {
      attachTo: document.body,
      props: {
        account: makeAccount({
          extra: {
            upstream_balance_probe: snapshot({
              data: {
                kind: 'openai_billing',
                remaining_usd: 62.75,
                total_usd: 100,
                used_usd: 37.25,
                observed_at: '2026-05-20T09:58:00Z'
              }
            })
          }
        }),
        now: NOW
      }
    })
    expect(wrapper.get('[data-testid="upstream-balance"]').text()).toBe('$62.75')
    expect(wrapper.find('[data-testid="upstream-balance-status"]').exists()).toBe(false)

    await wrapper.get('[data-testid="upstream-balance-details"]').trigger('mouseenter')
    await flushPromises()
    const tooltips = document.body.querySelectorAll('[role="tooltip"]')
    const tooltip = tooltips[tooltips.length - 1] as HTMLElement
    expect(tooltip.querySelector('[data-testid="upstream-balance-remaining"]')?.textContent).toContain('$62.75')
    expect(tooltip.querySelector('[data-testid="upstream-balance-total"]')?.textContent).toContain('$100.00')
    expect(tooltip.querySelector('[data-testid="upstream-balance-used"]')?.textContent).toContain('$37.25')
    expect(tooltip.querySelector('[data-testid="upstream-balance-kind"]')?.textContent).toContain(
      'admin.accounts.upstreamBalance.kindOpenAIBilling'
    )
    wrapper.unmount()
  })

  it('keeps the last known balance but flags it stale past fresh_until', () => {
    const wrapper = mountCell(
      makeAccount({
        extra: {
          upstream_balance_probe: snapshot({ fresh_until: '2026-05-20T09:59:00Z' })
        }
      })
    )
    expect(wrapper.get('[data-testid="upstream-balance"]').text()).toBe('$12.50')
    expect(wrapper.get('[data-testid="upstream-balance-status"]').text()).toBe('admin.accounts.upstreamBalance.stale')
  })

  it('shows unsupported without hiding the query button', () => {
    const wrapper = mountCell(
      makeAccount({
        extra: {
          upstream_balance_probe: snapshot({
            status: 'unsupported',
            data: undefined,
            last_error: 'unsupported'
          })
        }
      })
    )
    expect(wrapper.get('[data-testid="upstream-balance"]').text()).toContain('admin.accounts.upstreamBalance.unsupported')
    expect(wrapper.find('[data-testid="upstream-balance-probe"]').exists()).toBe(true)
  })

  it('renders a failed probe with its error and keeps any previously known balance', async () => {
    const failed = mount(UpstreamBalanceCell, {
      attachTo: document.body,
      props: {
        account: makeAccount({
          extra: {
            upstream_balance_probe: snapshot({ status: 'failed', last_error: 'http_error' })
          }
        }),
        now: NOW
      }
    })
    expect(failed.get('[data-testid="upstream-balance"]').text()).toBe('$12.50')
    expect(failed.get('[data-testid="upstream-balance-status"]').text()).toBe('admin.accounts.upstreamBalance.failed')

    await failed.get('[data-testid="upstream-balance-details"]').trigger('mouseenter')
    await flushPromises()
    const tooltips = document.body.querySelectorAll('[role="tooltip"]')
    const tooltip = tooltips[tooltips.length - 1] as HTMLElement
    expect(tooltip.querySelector('[data-testid="upstream-balance-error"]')?.textContent).toContain('http_error')
    failed.unmount()

    const withoutData = mountCell(
      makeAccount({
        extra: {
          upstream_balance_probe: snapshot({ status: 'failed', data: undefined, last_error: 'request_failed' })
        }
      })
    )
    expect(withoutData.get('[data-testid="upstream-balance"]').text()).toContain('admin.accounts.upstreamBalance.failed')
  })

  it('disables the query button while a probe is in flight and emits probe on click', async () => {
    const wrapper = mountCell(makeAccount(), true)
    const button = wrapper.get('[data-testid="upstream-balance-probe"]')
    expect(button.attributes('disabled')).toBeDefined()

    const idle = mountCell(makeAccount())
    await idle.get('[data-testid="upstream-balance-probe"]').trigger('click')
    expect(idle.emitted('probe')).toHaveLength(1)
  })

  // 计费模式必须是中文化的说明文案，而不是上游返回的枚举值。
  it('localizes the upstream billing mode instead of echoing the raw value', async () => {
    const wrapper = mount(UpstreamBalanceCell, {
      attachTo: document.body,
      props: {
        account: makeAccount({
          extra: {
            upstream_balance_probe: snapshot({
              data: {
                kind: 'sub2api',
                remaining_usd: 125.68,
                mode: 'unrestricted',
                observed_at: '2026-05-20T09:58:00Z'
              }
            })
          }
        }),
        now: NOW
      }
    })
    await wrapper.get('[data-testid="upstream-balance-details"]').trigger('mouseenter')
    await flushPromises()
    const tooltips = document.body.querySelectorAll('[role="tooltip"]')
    const tooltip = tooltips[tooltips.length - 1] as HTMLElement
    const mode = tooltip.querySelector('[data-testid="upstream-balance-mode"]')?.textContent ?? ''
    expect(mode).toContain('admin.accounts.upstreamBalance.modeUnrestricted')
    expect(mode).not.toContain('unrestricted:')
    wrapper.unmount()

    const limited = mount(UpstreamBalanceCell, {
      attachTo: document.body,
      props: {
        account: makeAccount({
          extra: {
            upstream_balance_probe: snapshot({
              data: {
                kind: 'sub2api',
                remaining_usd: 12.5,
                mode: 'quota_limited',
                observed_at: '2026-05-20T09:58:00Z'
              }
            })
          }
        }),
        now: NOW
      }
    })
    await limited.get('[data-testid="upstream-balance-details"]').trigger('mouseenter')
    await flushPromises()
    const limitedTooltips = document.body.querySelectorAll('[role="tooltip"]')
    const limitedTooltip = limitedTooltips[limitedTooltips.length - 1] as HTMLElement
    expect(limitedTooltip.querySelector('[data-testid="upstream-balance-mode"]')?.textContent).toContain(
      'admin.accounts.upstreamBalance.modeQuotaLimited'
    )
    limited.unmount()
  })

  // new-api 的不限量哨兵值不能显示成"还剩一亿美元"。
  it('renders an unlimited badge instead of a bogus huge balance', async () => {
    const wrapper = mount(UpstreamBalanceCell, {
      attachTo: document.body,
      props: {
        account: makeAccount({
          extra: {
            upstream_balance_probe: snapshot({
              data: { kind: 'newapi_token', unlimited: true, observed_at: '2026-05-20T09:58:00Z' }
            })
          }
        }),
        now: NOW
      }
    })
    const text = wrapper.get('[data-testid="upstream-balance"]').text()
    expect(text).toBe('admin.accounts.upstreamBalance.unlimited')
    expect(text).not.toContain('99,999')

    await wrapper.get('[data-testid="upstream-balance-details"]').trigger('mouseenter')
    await flushPromises()
    const tooltips = document.body.querySelectorAll('[role="tooltip"]')
    const tooltip = tooltips[tooltips.length - 1] as HTMLElement
    expect(tooltip.querySelector('[data-testid="upstream-balance-unlimited"]')).not.toBeNull()
    expect(tooltip.querySelector('[data-testid="upstream-balance-remaining"]')).toBeNull()
    wrapper.unmount()
  })

  it('toggles the per-account auto refresh opt-in', async () => {
    const wrapper = mount(UpstreamBalanceCell, {
      attachTo: document.body,
      props: { account: makeAccount(), now: NOW }
    })
    await wrapper.get('[data-testid="upstream-balance-details"]').trigger('mouseenter')
    await flushPromises()
    let tooltips = document.body.querySelectorAll('[role="tooltip"]')
    let tooltip = tooltips[tooltips.length - 1] as HTMLElement
    const toggle = tooltip.querySelector('[data-testid="upstream-balance-auto-toggle"]') as HTMLInputElement
    expect(toggle).not.toBeNull()
    expect(toggle.checked).toBe(false)

    toggle.click()
    await flushPromises()
    expect(wrapper.emitted('toggle-auto')).toEqual([[true]])
    wrapper.unmount()

    const enabled = mount(UpstreamBalanceCell, {
      attachTo: document.body,
      props: { account: makeAccount({ extra: { upstream_balance_probe_enabled: true } }), now: NOW }
    })
    await enabled.get('[data-testid="upstream-balance-details"]').trigger('mouseenter')
    await flushPromises()
    tooltips = document.body.querySelectorAll('[role="tooltip"]')
    tooltip = tooltips[tooltips.length - 1] as HTMLElement
    expect((tooltip.querySelector('[data-testid="upstream-balance-auto-toggle"]') as HTMLInputElement).checked).toBe(true)
    enabled.unmount()
  })

  it('shows the next auto refresh time only when the account opted in', async () => {
    const optedIn = mount(UpstreamBalanceCell, {
      attachTo: document.body,
      props: {
        account: makeAccount({
          extra: { upstream_balance_probe: snapshot(), upstream_balance_probe_enabled: true }
        }),
        now: NOW
      }
    })
    await optedIn.get('[data-testid="upstream-balance-details"]').trigger('mouseenter')
    await flushPromises()
    const tooltips = document.body.querySelectorAll('[role="tooltip"]')
    const tooltip = tooltips[tooltips.length - 1] as HTMLElement
    expect(tooltip.querySelector('[data-testid="upstream-balance-next-probe"]')).not.toBeNull()
    optedIn.unmount()

    const optedOut = mount(UpstreamBalanceCell, {
      attachTo: document.body,
      props: { account: makeAccount({ extra: { upstream_balance_probe: snapshot() } }), now: NOW }
    })
    await optedOut.get('[data-testid="upstream-balance-details"]').trigger('mouseenter')
    await flushPromises()
    const outTooltips = document.body.querySelectorAll('[role="tooltip"]')
    const outTooltip = outTooltips[outTooltips.length - 1] as HTMLElement
    expect(outTooltip.querySelector('[data-testid="upstream-balance-next-probe"]')).toBeNull()
    optedOut.unmount()
  })
})
