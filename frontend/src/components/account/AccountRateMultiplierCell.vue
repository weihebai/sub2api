<template>
  <div
    class="group/rate inline-flex h-7 items-center rounded-lg border transition-colors duration-150"
    :class="[
      editing
        ? 'border-primary-300 bg-primary-50/70 dark:border-primary-700 dark:bg-primary-900/20'
        : 'border-transparent hover:border-gray-200 hover:bg-gray-50 dark:hover:border-dark-600 dark:hover:bg-dark-700/60'
    ]"
    data-testid="account-rate-cell"
  >
    <input
      v-if="editing"
      ref="inputRef"
      v-model="inputValue"
      type="text"
      inputmode="decimal"
      class="h-full w-16 border-0 bg-transparent px-1 text-center font-mono text-sm tabular-nums text-gray-900 focus:outline-none focus:ring-0 dark:text-white"
      :aria-label="t('admin.accounts.columns.billingRateMultiplier')"
      data-testid="account-rate-input"
      @keydown.enter.prevent="commitInput"
      @keydown.esc.prevent="cancelInput"
      @keydown.up.prevent="nudgeInput(NUDGE_STEP)"
      @keydown.down.prevent="nudgeInput(-NUDGE_STEP)"
      @blur="commitInput"
    />
    <button
      v-else
      type="button"
      class="relative flex h-full items-center gap-1 rounded-lg px-1.5 font-mono text-sm tabular-nums transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-primary-500"
      :class="valueButtonClass"
      :disabled="saving"
      :title="valueTitle"
      :aria-label="t('admin.accounts.columns.billingRateMultiplier')"
      data-testid="account-rate-value"
      @click="startEditing"
    >
      <span>{{ formattedValue }}x</span>
      <span
        v-if="syncEnabled"
        class="inline-flex cursor-help text-emerald-600 dark:text-emerald-400"
        :aria-label="t('admin.accounts.upstreamBilling.syncedRateTooltip')"
        :title="t('admin.accounts.upstreamBilling.syncedRateTooltip')"
        data-testid="account-rate-sync-indicator"
      >
        <Icon name="sync" size="xs" />
      </span>
      <span
        v-if="saving"
        class="absolute -right-0.5 top-1 h-1.5 w-1.5 animate-pulse rounded-full bg-primary-500"
        data-testid="account-rate-saving"
      />
    </button>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { update as updateAccount } from '@/api/admin/accounts'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatMultiplier } from '@/utils/formatters'
import type { Account } from '@/types'

// 与账号编辑弹窗的 step="0.001" 同粒度，两处入口的微调精度保持一致
const NUDGE_STEP = 0.001

const props = defineProps<{ account: Account }>()
const emit = defineEmits<{
  (e: 'updated', account: Account): void
  (e: 'error', message: string): void
  (e: 'blocked', message: string): void
}>()

const { t } = useI18n()

// rate_multiplier 缺省按 1.0 口径展示与比较，与列表展示逻辑一致
const currentValue = computed(() => props.account.rate_multiplier ?? 1)
const syncEnabled = computed(() => props.account.extra?.upstream_billing_rate_sync_enabled === true)

const draft = ref(currentValue.value)
const saving = ref(false)
const editing = ref(false)
const inputValue = ref('')
const inputRef = ref<HTMLInputElement | null>(null)

const formattedValue = computed(() => formatMultiplier(draft.value))
const dirty = computed(() => draft.value !== currentValue.value)

watch(currentValue, (next) => {
  // 上游同步探测 / 自动刷新回写时跟随新值，但不打断正在进行的编辑
  if (!saving.value && !editing.value) draft.value = next
})

// 浮点运算噪声（如 0.1 + 0.002）在入库前收敛，避免把 0.10200000000000001 写进倍率
const normalize = (value: number) => Number(value.toPrecision(12))

const valueTitle = computed(() =>
  syncEnabled.value
    ? t('admin.accounts.upstreamBilling.syncRateManagedHint')
    : t('admin.accounts.rateQuick.editHint')
)

const valueButtonClass = computed(() => {
  if (dirty.value) return 'font-semibold text-primary-600 dark:text-primary-400'
  return 'text-gray-700 hover:text-gray-900 dark:text-gray-300 dark:hover:text-white'
})

const startEditing = () => {
  if (saving.value) return
  // 倍率归上游同步所有时后端会拒绝修改（UPSTREAM_BILLING_RATE_SYNC_CONFLICT），
  // 客户端直接拦下并提示先关闭同步，避免"点了改、保存报错"的体验断层。
  if (syncEnabled.value) {
    emit('blocked', t('admin.accounts.rateQuick.syncBlocked'))
    return
  }
  inputValue.value = String(currentValue.value)
  editing.value = true
  void nextTick(() => {
    inputRef.value?.focus()
    inputRef.value?.select()
  })
}

const nudgeInput = (delta: number) => {
  const parsed = Number.parseFloat(inputValue.value)
  const base = Number.isFinite(parsed) ? parsed : currentValue.value
  inputValue.value = String(normalize(Math.max(0, base + delta)))
}

const commitInput = () => {
  if (!editing.value) return
  editing.value = false
  const raw = inputValue.value.trim()
  const parsed = raw === '' ? Number.NaN : Number(raw)
  if (!Number.isFinite(parsed) || parsed < 0) {
    draft.value = currentValue.value
    emit('error', t('admin.accounts.rateQuick.invalid'))
    return
  }
  draft.value = normalize(parsed)
  void save()
}

const cancelInput = () => {
  editing.value = false
  if (!saving.value) draft.value = currentValue.value
}

const save = async () => {
  const target = draft.value
  if (target === currentValue.value) return
  saving.value = true
  try {
    const updated = await updateAccount(props.account.id, { rate_multiplier: target })
    emit('updated', updated)
  } catch (error) {
    draft.value = currentValue.value
    emit('error', extractApiErrorMessage(error, t('admin.accounts.rateQuick.failed'), {
      // 并发竞态兜底：点击后才被开启同步的账号仍会命中后端冲突，映射成本地化提示
      UPSTREAM_BILLING_RATE_SYNC_CONFLICT: t('admin.accounts.rateQuick.syncBlocked')
    }))
  } finally {
    saving.value = false
  }
}
</script>
