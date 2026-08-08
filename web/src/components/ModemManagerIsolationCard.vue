<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  Alert24Regular,
  ArrowSync24Regular,
  Delete24Regular,
  Server24Regular,
  Settings24Regular
} from '@vicons/fluent'
import {
  systemService,
  type ModemManagerIsolationDevice,
  type ModemManagerIsolationState,
  type ModemManagerIsolationStatus
} from '../services/system'
import { errorMessage } from '../services/http'

type TagType = 'success' | 'warning' | 'danger' | 'info'
type AlertType = 'success' | 'warning' | 'error' | 'info'

const status = ref<ModemManagerIsolationStatus | null>(null)
const loadingStatus = ref(false)
const operating = ref<'install' | 'uninstall' | null>(null)
const loadError = ref('')
let disposed = false

const stateMeta: Record<ModemManagerIsolationState, { label: string; tag: TagType; alert: AlertType; guide: string }> = {
  absent: {
    label: '未安装',
    tag: 'info',
    alert: 'info',
    guide: '尚未安装设备级隔离规则。安装后，ModemManager 只会忽略由 VoHive 接管的设备。'
  },
  current: {
    label: '已安装',
    tag: 'success',
    alert: 'success',
    guide: '隔离规则与当前 VoHive 设备一致。'
  },
  stale: {
    label: '需要更新',
    tag: 'warning',
    alert: 'warning',
    guide: 'VoHive 管理的设备已经变化，需要更新隔离规则。'
  },
  partial: {
    label: '覆盖不完整',
    tag: 'warning',
    alert: 'warning',
    guide: '部分设备无法生成精确匹配规则。请先处理下方提示，避免误以为所有设备都已隔离。'
  },
  pending_replug: {
    label: '等待重插',
    tag: 'warning',
    alert: 'warning',
    guide: '规则已经写入并重新加载。请只逐台重插由 VoHive 接管的设备，然后重新检查状态。'
  },
  foreign: {
    label: '外部规则',
    tag: 'danger',
    alert: 'error',
    guide: '检测到同名规则文件，但它不是由 VoHive 管理。为避免覆盖人工配置，网页操作已锁定。'
  },
  modified: {
    label: '文件已修改',
    tag: 'danger',
    alert: 'error',
    guide: 'VoHive 创建的规则已被外部修改。网页不会自动覆盖或删除，请先在宿主机检查文件。'
  },
  unsupported: {
    label: '不可用',
    tag: 'info',
    alert: 'info',
    guide: '当前部署不能从网页管理宿主机 udev 规则，请按部署文档在宿主机操作。'
  }
}

const manualAttentionMeta = {
  label: '需要人工确认',
  tag: 'danger' as TagType,
  alert: 'error' as AlertType,
  guide: '上一次宿主机配置的最终状态无法安全确认。网页操作已锁定，请按提示恢复后再继续。'
}

const currentState = computed<ModemManagerIsolationState>(() => status.value?.status || 'unsupported')
const currentMeta = computed(() => status.value?.manual_attention ? manualAttentionMeta : stateMeta[currentState.value])
const devices = computed(() => Array.isArray(status.value?.devices) ? status.value.devices : [])

const totalDevices = computed(() => {
  const total = status.value?.total_devices
  return typeof total === 'number' && total >= 0 ? total : devices.value.length
})

const coveredDevices = computed(() => {
  const covered = status.value?.covered_devices
  if (typeof covered === 'number' && covered >= 0) return covered
  return devices.value.filter((device) => device.covered).length
})

const canInstall = computed(() => {
  if (!status.value) return false
  if (typeof status.value.can_install === 'boolean') return status.value.can_install
  return currentState.value === 'absent' || currentState.value === 'stale'
})

const canUninstall = computed(() => {
  if (!status.value) return false
  if (typeof status.value.can_uninstall === 'boolean') return status.value.can_uninstall
  if (status.value.managed_by_vohive === false) return false
  return ['current', 'stale', 'partial', 'pending_replug'].includes(currentState.value)
})

const showInstall = computed(() => {
  return canInstall.value || ['absent', 'stale', 'partial'].includes(currentState.value)
})

const showUninstall = computed(() => {
  return canUninstall.value || ['current', 'stale', 'partial', 'pending_replug'].includes(currentState.value)
})

const installLabel = computed(() => currentState.value === 'absent' ? '安装隔离配置' : '更新隔离配置')

function selectorLabel(device: ModemManagerIsolationDevice): string {
  switch (device.selector_kind) {
    case 'usb_serial':
      return 'USB 唯一序列号'
    case 'usb_path':
      return '固定物理端口'
    default:
      return '尚未取得精确匹配条件'
  }
}

function deviceEffectLabel(device: ModemManagerIsolationDevice): { label: string; type: TagType } {
  if (device.covered) return { label: '规则已覆盖', type: 'info' }
  return { label: '未覆盖', type: 'danger' }
}

function isCancel(error: unknown): boolean {
  return error === 'cancel' || error === 'close'
}

async function fetchStatus(showError = true): Promise<boolean> {
  if (disposed) return false
  loadingStatus.value = true
  const result = await systemService.getModemManagerIsolation()
  if (disposed) return false
  loadingStatus.value = false
  if (!result.ok) {
    loadError.value = result.error.message || '读取 ModemManager 隔离状态失败'
    if (showError) ElMessage.error(loadError.value)
    return false
  }
  status.value = result.data
  loadError.value = ''
  return true
}

async function promptCurrentPassword(actionLabel: string): Promise<string | null> {
  try {
    const authorization = await ElMessageBox.prompt(
      `长期登录会话不能直接${actionLabel}宿主机配置。请输入当前管理员密码以确认本次操作。`,
      '验证管理员身份',
      {
        confirmButtonText: `验证并${actionLabel}`,
        cancelButtonText: '取消',
        inputType: 'password',
        inputPlaceholder: '当前管理员密码',
        closeOnClickModal: false,
        inputValidator: (value: string) => value.length > 0 || '请输入当前管理员密码'
      }
    )
    return String(authorization.value || '')
  } catch (error: unknown) {
    if (isCancel(error)) return null
    throw error
  }
}

async function installIsolation() {
  if (!status.value || !canInstall.value || operating.value) return
  let currentPassword = ''
  let requestStarted = false
  try {
    await ElMessageBox.confirm(
      [
        `将为后端识别出的 ${totalDevices.value} 台 VoHive 设备生成精确隔离规则。`,
        '',
        '此操作不会全局停用或重启 ModemManager，也不会触发主机上的其他 USB 设备。',
        '规则写入后通常需要逐台重插目标设备，设备会短暂掉线。'
      ].join('\n'),
      currentState.value === 'absent' ? '安装 ModemManager 隔离配置' : '更新 ModemManager 隔离配置',
      {
        confirmButtonText: currentState.value === 'absent' ? '继续安装' : '继续更新',
        cancelButtonText: '取消',
        type: 'warning'
      }
    )
    const password = await promptCurrentPassword(currentState.value === 'absent' ? '安装' : '更新')
    if (password === null || disposed) return
    currentPassword = password
    operating.value = 'install'
    requestStarted = true
    const result = await systemService.installModemManagerIsolation({
      current_password: currentPassword,
      expected_revision: status.value.revision || '',
      expected_plan_revision: status.value.plan_revision || ''
    })
    currentPassword = ''
    if (disposed) return
    if (!result.ok) throw result.error
    status.value = result.data
    loadError.value = ''
    ElMessage.success(result.data.message || '隔离规则已写入，请按状态提示完成逐台重插')
  } catch (error: unknown) {
    if (isCancel(error)) return
    ElMessage.error(errorMessage(error, '安装隔离配置失败'))
    if (requestStarted) await fetchStatus(false)
  } finally {
    currentPassword = ''
    operating.value = null
  }
}

async function uninstallIsolation() {
  if (!status.value || !canUninstall.value || operating.value) return
  let currentPassword = ''
  let requestStarted = false
  try {
    await ElMessageBox.confirm(
      [
        '将删除由 VoHive 管理的 ModemManager 设备隔离规则。',
        '',
        '下次重插后，ModemManager 可能重新接管这些设备；若 VoHive 仍启用 QMI，所有权预检会拒绝启动。',
        '此操作不会自动重启 ModemManager，也不会触发主机上的其他 USB 设备。'
      ].join('\n'),
      '卸载 ModemManager 隔离配置',
      {
        confirmButtonText: '继续卸载',
        cancelButtonText: '取消',
        type: 'warning'
      }
    )
    const password = await promptCurrentPassword('卸载')
    if (password === null || disposed) return
    currentPassword = password
    operating.value = 'uninstall'
    requestStarted = true
    const result = await systemService.uninstallModemManagerIsolation({
      current_password: currentPassword,
      expected_revision: status.value.revision || '',
      expected_plan_revision: status.value.plan_revision || ''
    })
    currentPassword = ''
    if (disposed) return
    if (!result.ok) throw result.error
    status.value = result.data
    loadError.value = ''
    ElMessage.success(result.data.message || '隔离规则已卸载，请按状态提示完成逐台重插')
  } catch (error: unknown) {
    if (isCancel(error)) return
    ElMessage.error(errorMessage(error, '卸载隔离配置失败'))
    if (requestStarted) await fetchStatus(false)
  } finally {
    currentPassword = ''
    operating.value = null
  }
}

onMounted(() => {
  disposed = false
  void fetchStatus(false)
})

onBeforeUnmount(() => {
  disposed = true
})
</script>

<template>
  <div class="ui-card p-8 relative overflow-hidden group lg:col-span-2">
    <div class="absolute top-0 right-0 w-40 h-40 bg-cyan-500/5 rounded-bl-full -mr-10 -mt-10 transition-transform group-hover:scale-110"></div>

    <div class="flex flex-col sm:flex-row sm:items-start sm:justify-between gap-4 mb-6 relative z-10">
      <div class="flex items-center gap-3 min-w-0">
        <div class="w-12 h-12 rounded-xl bg-cyan-50 dark:bg-cyan-500/10 flex items-center justify-center text-cyan-600 dark:text-cyan-400 shrink-0">
          <el-icon size="24"><Server24Regular /></el-icon>
        </div>
        <div class="min-w-0">
          <div class="flex flex-wrap items-center gap-2">
            <h3 class="text-lg font-bold text-gray-800 dark:text-gray-100">ModemManager 混合使用</h3>
            <el-tag v-if="status" size="small" :type="currentMeta.tag">{{ currentMeta.label }}</el-tag>
          </div>
          <p class="text-xs text-gray-500 mt-1">
            只让 ModemManager 忽略由 VoHive 接管的设备，其他蜂窝设备不受影响
          </p>
        </div>
      </div>
      <el-button
        :loading="loadingStatus"
        :disabled="!!operating"
        class="self-start sm:self-center shrink-0"
        @click="fetchStatus()"
      >
        <el-icon><ArrowSync24Regular /></el-icon>
        重新检查
      </el-button>
    </div>

    <div class="relative z-10 space-y-4">
      <el-alert
        v-if="loadError"
        type="error"
        :closable="false"
        show-icon
        :title="loadError"
      />

      <div v-if="!status && loadingStatus" class="ui-panel-muted p-6 text-sm text-gray-500">
        正在检查宿主机隔离配置…
      </div>

      <template v-if="status">
        <el-alert
          :type="currentMeta.alert"
          :closable="false"
          show-icon
          :title="status.reason || currentMeta.guide"
        />

        <el-alert
          v-if="status.warning"
          type="warning"
          :closable="false"
          show-icon
          :title="status.warning"
        />

        <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <div class="ui-panel-muted p-4">
            <div class="text-xs text-gray-500 mb-1">设备覆盖</div>
            <div class="text-sm font-bold text-gray-800 dark:text-gray-100">
              {{ coveredDevices }} / {{ totalDevices }}
            </div>
          </div>
          <div class="ui-panel-muted p-4 min-w-0">
            <div class="text-xs text-gray-500 mb-1">规则文件</div>
            <div class="text-sm font-mono text-gray-800 dark:text-gray-100 truncate" :title="status.rule_path || ''">
              {{ status.rule_path || '--' }}
            </div>
          </div>
        </div>

        <div v-if="devices.length" class="border border-gray-200 dark:border-white/10 rounded-xl overflow-hidden">
          <div
            v-for="device in devices"
            :key="device.device_id"
            class="p-4 flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3 border-b border-gray-100 dark:border-white/5 last:border-b-0"
          >
            <div class="min-w-0">
              <div class="text-sm font-bold text-gray-800 dark:text-gray-100 truncate">
                {{ device.name || device.device_id }}
              </div>
              <div class="text-xs text-gray-500 mt-1">
                {{ selectorLabel(device) }}
                <span v-if="device.selector_value" class="font-mono"> · {{ device.selector_value }}</span>
              </div>
              <div v-if="device.reason" class="text-xs text-amber-700 dark:text-amber-300 mt-1">
                {{ device.reason }}
              </div>
            </div>
            <el-tag size="small" :type="deviceEffectLabel(device).type" class="self-start sm:self-center shrink-0">
              {{ deviceEffectLabel(device).label }}
            </el-tag>
          </div>
        </div>

        <div class="flex flex-col-reverse sm:flex-row sm:items-center sm:justify-end gap-3 pt-2">
          <el-button
            v-if="showUninstall"
            type="danger"
            plain
            :loading="operating === 'uninstall'"
            :disabled="!!operating || loadingStatus || !canUninstall"
            @click="uninstallIsolation"
          >
            <el-icon><Delete24Regular /></el-icon>
            卸载隔离配置
          </el-button>
          <el-button
            v-if="showInstall"
            type="primary"
            :loading="operating === 'install'"
            :disabled="!!operating || loadingStatus || !canInstall"
            class="!border-0"
            @click="installIsolation"
          >
            <el-icon><Settings24Regular /></el-icon>
            {{ installLabel }}
          </el-button>
        </div>

        <div
          v-if="status.manual_attention"
          class="text-xs text-red-600 dark:text-red-300 flex items-start gap-2"
        >
          <el-icon class="mt-0.5 shrink-0"><Alert24Regular /></el-icon>
          <span>在完成上方恢复步骤前，网页会保持锁定，不会继续修改宿主机配置。</span>
        </div>
        <div
          v-else-if="currentState === 'foreign' || currentState === 'modified'"
          class="text-xs text-red-600 dark:text-red-300 flex items-start gap-2"
        >
          <el-icon class="mt-0.5 shrink-0"><Alert24Regular /></el-icon>
          <span>请先在宿主机检查现有规则文件；网页不会覆盖或删除无法确认所有权的配置。</span>
        </div>
      </template>
    </div>
  </div>
</template>
