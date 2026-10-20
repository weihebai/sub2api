<template>
  <div v-if="eligible" class="flex h-6 min-w-[7rem] items-center gap-1" data-testid="upstream-balance-cell">
    <HelpTooltip class="-ml-1" width-class="w-max max-w-[calc(100vw-2rem)]" data-testid="upstream-balance-details">
      <template #trigger>
        <span
          class="cursor-help border-b border-dotted border-gray-300 text-sm font-medium dark:border-dark-600"
          :class="valueClass"
          data-testid="upstream-balance"
        >
          {{ primaryValue }}
        </span>
      </template>
      <div class="space-y-1">
        <p v-if="hasBalance" data-testid="upstream-balance-remaining">
          {{ t('admin.accounts.upstreamBalance.remaining', { value: balanceLabel }) }}
        </p>
        <p v-if="isUnlimited" data-testid="upstream-balance-unlimited">
          {{ t('admin.accounts.upstreamBalance.unlimitedHint') }}
        </p>
        <p v-if="typeof data?.upstream_used_raw === 'number'" data-testid="upstream-balance-raw-used">
          {{ t('admin.accounts.upstreamBalance.upstreamUsedRaw', { value: formatRaw(data.upstream_used_raw) }) }}
        </p>
        <p v-if="typeof data?.total_usd === 'number'" data-testid="upstream-balance-total">
          {{ t('admin.accounts.upstreamBalance.total', { value: formatUSD(data.total_usd) }) }}
        </p>
        <p v-if="typeof data?.used_usd === 'number'" data-testid="upstream-balance-used">
          {{ t('admin.accounts.upstreamBalance.used', { value: formatUSD(data.used_usd) }) }}
        </p>
        <p v-if="kind" data-testid="upstream-balance-kind">{{ kindLabel }}</p>
        <p v-if="modeLabel" data-testid="upstream-balance-mode">
          {{ t('admin.accounts.upstreamBalance.mode', { value: modeLabel }) }}
        </p>
        <p v-if="quotaLabel" data-testid="upstream-balance-quota">{{ quotaLabel }}</p>
        <p v-if="snapshot" data-testid="upstream-balance-updated">
          {{ t('admin.accounts.upstreamBalance.updatedAt', { value: formatDate(snapshot.received_at) }) }}
        </p>
        <p v-if="nextProbeLabel" data-testid="upstream-balance-next-probe">
          {{ t('admin.accounts.upstreamBalance.nextProbeAt', { value: nextProbeLabel }) }}
        </p>
        <p v-if="snapshot?.last_error" class="text-red-400" data-testid="upstream-balance-error">
          {{ t('admin.accounts.upstreamBalance.lastError', { value: snapshot.last_error }) }}
        </p>
        <p class="border-t border-gray-200 pt-1 dark:border-dark-600">
          <label class="flex cursor-pointer items-center gap-1.5 text-xs" data-testid="upstream-balance-auto-label">
            <input
              type="checkbox"
              class="h-3 w-3 rounded border-gray-300"
              :checked="autoProbeEnabled"
              :disabled="savingAuto"
              data-testid="upstream-balance-auto-toggle"
              @change="$emit('toggle-auto', ($event.target as HTMLInputElement).checked)"
            />
            <span>{{ t('admin.accounts.upstreamBalance.autoProbe') }}</span>
          </label>
        </p>
      </div>
    </HelpTooltip>
    <span
      v-if="statusLabel"
      :class="statusClass"
      class="whitespace-nowrap text-[10px] font-medium"
      data-testid="upstream-balance-status"
    >
      {{ statusLabel }}
    </span>
    <button
      type="button"
      class="inline-flex h-6 flex-shrink-0 items-center gap-0.5 whitespace-nowrap rounded px-1.5 text-[10px] font-medium text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
      :disabled="probing"
      :title="t('admin.accounts.upstreamBalance.probeTooltip')"
      data-testid="upstream-balance-probe"
      @click="$emit('probe')"
    >
      <svg class="h-2.5 w-2.5" :class="{ 'animate-spin': probing }" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path
          stroke-linecap="round"
          stroke-linejoin="round"
          stroke-width="2"
          d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2H15"
        />
      </svg>
      {{ t('admin.accounts.upstreamBalance.probe') }}
    </button>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import type { Account, UpstreamBalanceProbeSnapshot } from '@/types'

const props = defineProps<{
  account: Account
  now: number
  probing?: boolean
  savingAuto?: boolean
}>()

defineEmits<{
  (event: 'probe'): void
  (event: 'toggle-auto', enabled: boolean): void
}>()

const { t } = useI18n()
const CLOCK_SKEW_TOLERANCE_MS = 5 * 60 * 1000

// 与倍率探测同样的资格口径：只有 API-key 账号才持有可查询的静态上游密钥。
const eligible = computed(() => props.account.type === 'apikey')
const snapshot = computed<UpstreamBalanceProbeSnapshot | undefined>(
  () => props.account.extra?.upstream_balance_probe
)
const data = computed(() => snapshot.value?.data)
const kind = computed(() => data.value?.kind)
const kindLabel = computed(() => {
  switch (kind.value) {
    case 'sub2api':
      return t('admin.accounts.upstreamBalance.kindSub2API')
    case 'newapi_token':
      return t('admin.accounts.upstreamBalance.kindNewAPIToken')
    default:
      return t('admin.accounts.upstreamBalance.kindOpenAIBilling')
  }
})
const autoProbeEnabled = computed(() => props.account.extra?.upstream_balance_probe_enabled === true)

// 计费模式中文化：不再是原始 API 枚举值。
const modeLabel = computed(() => {
  const mode = data.value?.mode
  if (!mode) return ''
  if (mode === 'unrestricted') return t('admin.accounts.upstreamBalance.modeUnrestricted')
  if (mode === 'quota_limited') return t('admin.accounts.upstreamBalance.modeQuotaLimited')
  return mode
})
const quotaLabel = computed(() => {
  const limit = data.value?.quota_limit_usd
  if (typeof limit !== 'number') return ''
  return t('admin.accounts.upstreamBalance.quota', {
    limit: formatUSD(limit),
    used: formatUSD(data.value?.quota_used_usd ?? 0)
  })
})

const receivedAt = computed(() =>
  typeof snapshot.value?.received_at === 'string' ? Date.parse(snapshot.value.received_at) : Number.NaN
)
const freshUntil = computed(() => {
  if (typeof snapshot.value?.fresh_until === 'string') return Date.parse(snapshot.value.fresh_until)
  if (snapshot.value?.status !== 'ok' || typeof snapshot.value.next_probe_at !== 'string') return Number.NaN
  const nextProbeAt = Date.parse(snapshot.value.next_probe_at)
  return Number.isFinite(nextProbeAt) && nextProbeAt > receivedAt.value
    ? receivedAt.value + 2 * (nextProbeAt - receivedAt.value)
    : Number.NaN
})
const validTimestamps = computed(() => {
  if (!Number.isFinite(receivedAt.value) || receivedAt.value > props.now + CLOCK_SKEW_TOLERANCE_MS) return false
  return Number.isFinite(freshUntil.value) && freshUntil.value > receivedAt.value
})
const stale = computed(() => {
  if (!snapshot.value) return false
  if (!Number.isFinite(receivedAt.value)) return snapshot.value.status === 'ok'
  if (!validTimestamps.value) return true
  return props.now > freshUntil.value
})

const remaining = computed(() => {
  const value = data.value?.remaining_usd
  return typeof value === 'number' && Number.isFinite(value) ? value : null
})
const hasBalance = computed(() => remaining.value !== null)
const balanceLabel = computed(() => (remaining.value === null ? '-' : formatUSD(remaining.value)))
const isUnlimited = computed(() => data.value?.unlimited === true && remaining.value === null)

// 余额是"最后一次已知值"仍然有意义的指标：过期或探测失败时保留数值，
// 只用状态标签提示新鲜度，而不是像倍率那样退回 "-"。
const primaryValue = computed(() => {
  if (!snapshot.value) return t('admin.accounts.upstreamBalance.notProbed')
  if (snapshot.value.status === 'unsupported') return t('admin.accounts.upstreamBalance.unsupported')
  if (hasBalance.value) return balanceLabel.value
  if (isUnlimited.value) return t('admin.accounts.upstreamBalance.unlimited')
  if (stale.value) return t('admin.accounts.upstreamBalance.stale')
  if (snapshot.value.status === 'failed') return t('admin.accounts.upstreamBalance.failed')
  return '-'
})

const statusLabel = computed(() => {
  if (!snapshot.value || !hasBalance.value) return ''
  if (stale.value) return t('admin.accounts.upstreamBalance.stale')
  if (snapshot.value.status === 'failed') return t('admin.accounts.upstreamBalance.failed')
  return ''
})

const valueClass = computed(() => {
  if (!snapshot.value || snapshot.value.status === 'unsupported') return 'text-gray-400 dark:text-gray-500'
  if (snapshot.value.status === 'failed') return 'font-mono text-red-600 dark:text-red-400'
  if (stale.value) return 'font-mono text-amber-600 dark:text-amber-400'
  if (isUnlimited.value) return 'text-gray-500 dark:text-gray-400'
  return 'font-mono text-gray-800 dark:text-gray-200'
})
const statusClass = computed(() =>
  stale.value ? 'text-amber-600 dark:text-amber-400' : 'text-red-600 dark:text-red-400'
)

const nextProbeLabel = computed(() => {
  if (!autoProbeEnabled.value) return ''
  const value = snapshot.value?.next_probe_at
  return typeof value === 'string' && Number.isFinite(Date.parse(value)) ? formatDate(value) : ''
})

const formatUSD = (value: number) =>
  `$${value.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`
// 上游自己的计费数字：单位因站点而异，不加货币符号，避免误导成美元。
const formatRaw = (value: number) =>
  value.toLocaleString('en-US', { minimumFractionDigits: 0, maximumFractionDigits: 4 })
const formatDate = (value?: string) =>
  value
    ? new Date(value).toLocaleString(undefined, {
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit'
      })
    : '-'
</script>
