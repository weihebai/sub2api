import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountRateMultiplierCell from '../AccountRateMultiplierCell.vue'
import type { Account } from '@/types'
import { update } from '@/api/admin/accounts'

vi.mock('@/api/admin/accounts', () => ({ update: vi.fn() }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const account = (overrides: Partial<Account> = {}) => ({
  id: 7, name: 'Claude 1', platform: 'anthropic', type: 'apikey', priority: 1,
  rate_multiplier: 0.28,
  ...overrides,
}) as Account

const mountCell = (value = account()) => mount(AccountRateMultiplierCell, { props: { account: value } })

const startEditing = async (wrapper: ReturnType<typeof mountCell>) => {
  await wrapper.get('[data-testid="account-rate-value"]').trigger('click')
  return wrapper.get('[data-testid="account-rate-input"]')
}

beforeEach(() => {
  vi.mocked(update).mockReset().mockImplementation(async (id, req) => account({ id, rate_multiplier: req.rate_multiplier }))
})

describe('AccountRateMultiplierCell', () => {
  it('renders the formatted rate and saves a typed value on Enter', async () => {
    const wrapper = mountCell()
    expect(wrapper.get('[data-testid="account-rate-value"]').text()).toBe('0.28x')

    const input = await startEditing(wrapper)
    await input.setValue('0.25')
    await input.trigger('keydown', { key: 'Enter' })
    await flushPromises()

    expect(update).toHaveBeenCalledTimes(1)
    expect(update).toHaveBeenCalledWith(7, { rate_multiplier: 0.25 })
    expect(wrapper.emitted('updated')?.[0]?.[0]).toMatchObject({ id: 7, rate_multiplier: 0.25 })
  })

  it('commits on blur and ignores unchanged input', async () => {
    const wrapper = mountCell()
    const input = await startEditing(wrapper)
    await input.setValue('0.25')
    await input.trigger('blur')
    await flushPromises()
    expect(update).toHaveBeenCalledWith(7, { rate_multiplier: 0.25 })

    // 真实页面里 handleAccountUpdated 会把保存结果回填到行数据
    await wrapper.setProps({ account: account({ rate_multiplier: 0.25 }) })
    vi.mocked(update).mockClear()
    const input2 = await startEditing(wrapper)
    await input2.setValue('0.25')
    await input2.trigger('blur')
    await flushPromises()
    expect(update).not.toHaveBeenCalled()
  })

  it('Escape cancels typing without saving', async () => {
    const wrapper = mountCell()
    const input = await startEditing(wrapper)
    await input.setValue('9.99')
    await input.trigger('keydown', { key: 'Escape' })
    await flushPromises()
    expect(update).not.toHaveBeenCalled()
    expect(wrapper.get('[data-testid="account-rate-value"]').text()).toBe('0.28x')
  })

  it('nudges the input by 0.001 with arrow keys before committing', async () => {
    const wrapper = mountCell()
    const input = await startEditing(wrapper)
    await input.trigger('keydown', { key: 'ArrowUp' })
    await input.trigger('keydown', { key: 'ArrowUp' })
    await input.trigger('keydown', { key: 'ArrowDown' })
    expect((input.element as HTMLInputElement).value).toBe('0.281')
    await input.trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(update).toHaveBeenCalledWith(7, { rate_multiplier: 0.281 })
  })

  it('rejects negative or non-numeric input without calling the API', async () => {
    const wrapper = mountCell()
    const input = await startEditing(wrapper)
    await input.setValue('-1')
    await input.trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(update).not.toHaveBeenCalled()
    expect(wrapper.emitted('error')).toHaveLength(1)
    expect(wrapper.get('[data-testid="account-rate-value"]').text()).toBe('0.28x')

    const input2 = await startEditing(wrapper)
    await input2.setValue('abc')
    await input2.trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(update).not.toHaveBeenCalled()
    expect(wrapper.emitted('error')).toHaveLength(2)
  })

  it('reverts and emits an error when the update fails', async () => {
    vi.mocked(update).mockRejectedValueOnce(new Error('boom'))
    const wrapper = mountCell()
    const input = await startEditing(wrapper)
    await input.setValue('0.5')
    await input.trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(wrapper.get('[data-testid="account-rate-value"]').text()).toBe('0.28x')
    expect(wrapper.emitted('error')).toHaveLength(1)
    expect(wrapper.emitted('updated')).toBeUndefined()
  })

  it('keeps the sync indicator and blocks editing while upstream rate sync is enabled', async () => {
    const wrapper = mountCell({
      extra: { upstream_billing_rate_sync_enabled: true }
    } as Partial<Account>)
    const indicator = wrapper.get('[data-testid="account-rate-sync-indicator"]')
    expect(indicator.attributes('title')).toBe('admin.accounts.upstreamBilling.syncedRateTooltip')

    await wrapper.get('[data-testid="account-rate-value"]').trigger('click')
    expect(wrapper.find('[data-testid="account-rate-input"]').exists()).toBe(false)
    expect(update).not.toHaveBeenCalled()
    expect(wrapper.emitted('blocked')?.[0]?.[0]).toBe('admin.accounts.rateQuick.syncBlocked')
  })

  it('follows externally refreshed rate values while idle', async () => {
    const wrapper = mountCell()
    await wrapper.setProps({ account: account({ rate_multiplier: 0.18 }) })
    expect(wrapper.get('[data-testid="account-rate-value"]').text()).toBe('0.18x')
  })
})
