import { api } from '../stores/auth'
import { callService } from './http'

export type DocsLinks = {
  swagger_ui: string
  openapi_yaml: string
  openapi_json: string
}

export type UpdateChannel = 'stable' | 'beta' | 'pinned'

export type UpdateCapabilities = {
  channel: 'stable' | 'beta' | 'pinned'
  install_type: 'systemd' | 'openwrt' | 'portable'
  layout: 'v1' | 'v2'
  can_check: boolean
  can_update: boolean
  can_rollback: boolean
  reason?: string
}

export type UpdateCandidate = {
  has_update: boolean
  current_version: string
  latest_version: string
  release_note: string
  manifest: {
    version: string
    channel: 'stable' | 'beta'
  }
}

export type UpdateCheckResponse = {
  capabilities: UpdateCapabilities
  candidate?: UpdateCandidate
}

export type UpdatePhase =
  | 'checking'
  | 'downloading'
  | 'verifying'
  | 'waiting_for_quiesce'
  | 'backing_up'
  | 'stopping'
  | 'switching'
  | 'starting'
  | 'verifying_service'
  | 'completed'
  | 'rolling_back'
  | 'rolled_back'
  | 'failed'
  | 'manual_recovery_required'

export type UpdateTransaction = {
  schema: number
  id: string
  operation: string
  phase: UpdatePhase
  current_version?: string
  target_version?: string
  error?: string
  rollback_error?: string
  started_at: string
  updated_at: string
}
export type SystemInfo = {
  version: string
  build_time: string
  config: string
  docs: DocsLinks
}

export type ModemManagerIsolationState =
  | 'absent'
  | 'current'
  | 'stale'
  | 'partial'
  | 'pending_replug'
  | 'foreign'
  | 'modified'
  | 'unsupported'

export type ModemManagerIsolationSelectorKind = 'usb_serial' | 'usb_path' | 'none'

export type ModemManagerIsolationDevice = {
  device_id: string
  name?: string
  control_device?: string
  selector_kind?: ModemManagerIsolationSelectorKind
  selector_value?: string
  covered?: boolean
  reason?: string
}

export type ModemManagerIsolationStatus = {
  status: ModemManagerIsolationState
  reason?: string
  rule_path?: string
  revision?: string
  plan_revision?: string
  managed_by_vohive?: boolean
  can_install?: boolean
  can_uninstall?: boolean
  total_devices?: number
  covered_devices?: number
  requires_replug?: boolean
  warning?: string
  manual_attention?: boolean
  devices?: ModemManagerIsolationDevice[]
}

export type ModemManagerIsolationActionRequest = {
  current_password: string
  expected_revision: string
  expected_plan_revision: string
}

export type ModemManagerIsolationActionResponse = ModemManagerIsolationStatus & {
  message?: string
}

export type TelegramSettings = {
  enabled: boolean
  bot_token: string
  chat_id: number | null
  admin_id: number | null
  base_url: string
  proxy: string
}

export type FeishuSettings = {
  enabled: boolean
  app_id: string
  app_secret: string
  chat_ids: string[]
}

export type QQSettings = {
  enabled: boolean
  app_id: string
  app_secret: string
  group_ids: string
  direct_ids: string
}

export type WebhookSettings = {
  enabled: boolean
  urls: string[]
  secret: string
  timeout_ms: number
  retry_max: number
  text_template: string
  headers: Record<string, string>
}

export type BarkSettings = {
  enabled: boolean
  urls: string[]
  group: string
  icon: string
  level: string
}

export type EmailSettings = {
  enabled: boolean
  use_ssl: boolean
  smtp_host: string
  smtp_port: number
  username: string
  password: string
  from_address: string
  to_addresses: string[]
}

export type PushplusSettings = {
  enabled: boolean
  token: string
  topic: string
  channel: string
}

export type NotificationsSettingsResponse = {
  telegram?: Partial<TelegramSettings>
  feishu?: Partial<FeishuSettings>
  qq?: Partial<QQSettings>
  email?: Partial<EmailSettings>
  pushplus?: Partial<PushplusSettings>
  webhook?: Partial<WebhookSettings>
  bark?: Partial<BarkSettings>
}

export type SaveNotificationsPayload = {
  telegram: {
    enabled: boolean
    bot_token: string
    chat_id: number
    admin_id: number
    base_url: string
    proxy: string
  }
  feishu: {
    enabled: boolean
    app_id: string
    app_secret: string
    chat_ids: string[]
  }
  qq: {
    enabled: boolean
    app_id: string
    app_secret: string
    group_ids: string
    direct_ids: string
  }
  email: {
    enabled: boolean
    use_ssl: boolean
    smtp_host: string
    smtp_port: number
    username: string
    password: string
    from_address: string
    to_addresses: string[]
  }
  pushplus: {
    enabled: boolean
    token: string
    topic: string
    channel: string
  }
  webhook: {
    enabled: boolean
    urls: string[]
    secret: string
    timeout_ms: number
    retry_max: number
    text_template: string
    headers?: Record<string, string>
  }
  bark: {
    enabled: boolean
    urls: string[]
    group: string
    icon: string
    level: string
  }
}

export type SaveNotificationsResponse = {
  applied?: boolean
  warning?: string
}

export type TestWebhookPayload = {
  enabled: boolean
  urls: string[]
  secret: string
  timeout_ms: number
  retry_max: number
  text_template: string
  headers?: Record<string, string>
}

export type TestWebhookResponse = {
  ok: boolean
  message: string
  failed_urls?: string[]
}

export type TestBarkPayload = {
  enabled: boolean
  urls: string[]
  group: string
  icon: string
  level: string
}

export type TestBarkResponse = {
  ok: boolean
  message: string
  failed_urls?: string[]
}

export type TestEmailPayload = {
  enabled: boolean
  use_ssl: boolean
  smtp_host: string
  smtp_port: number
  username: string
  password: string
  from_address: string
  to_addresses: string[]
}

export type TestEmailResponse = {
  ok: boolean
  message: string
}

export const systemService = {
  getInfo() {
    return callService(async () => {
      const res = await api.get('/system/info')
      return res.data as SystemInfo
    })
  },
  getModemManagerIsolation() {
    return callService(async () => {
      const res = await api.get<ModemManagerIsolationStatus>('/system/modemmanager/isolation')
      return res.data
    })
  },
  installModemManagerIsolation(payload: ModemManagerIsolationActionRequest) {
    return callService(async () => {
      const res = await api.post<ModemManagerIsolationActionResponse>(
        '/system/modemmanager/isolation/actions/install',
        payload
      )
      return res.data || {}
    })
  },
  uninstallModemManagerIsolation(payload: ModemManagerIsolationActionRequest) {
    return callService(async () => {
      const res = await api.post<ModemManagerIsolationActionResponse>(
        '/system/modemmanager/isolation/actions/uninstall',
        payload
      )
      return res.data || {}
    })
  },
  changePassword(payload: { old_password: string; new_password: string; confirm_password: string }) {
    return callService(async () => {
      await api.post('/settings/password', payload)
      return true
    })
  },
  getNotifications() {
    return callService(async () => {
      const res = await api.get('/settings/notifications')
      return (res.data || {}) as NotificationsSettingsResponse
    })
  },
  saveNotifications(payload: SaveNotificationsPayload) {
    return callService(async () => {
      const res = await api.put<SaveNotificationsResponse>('/settings/notifications', payload)
      return {
        applied: res.data?.applied,
        warning: res.data?.warning
      }
    })
  },
  testWebhook(payload: TestWebhookPayload) {
    return callService(async () => {
      const res = await api.post<TestWebhookResponse>('/settings/notifications/webhook/test', payload)
      return res.data
    })
  },
  testBark(payload: TestBarkPayload) {
    return callService(async () => {
      const res = await api.post<TestBarkResponse>('/settings/notifications/bark/test', payload)
      return res.data
    })
  },
  testEmail(payload: TestEmailPayload) {
    return callService(async () => {
      const res = await api.post<TestEmailResponse>('/settings/notifications/email/test', payload)
      return res.data
    })
  },
  getUpdateCapabilities() {
    return callService(async () => {
      const res = await api.get<UpdateCapabilities>('/system/update/capabilities')
      return res.data
    })
  },
  checkUpdate(channel?: 'stable' | 'beta') {
    return callService(async () => {
      const res = await api.get<UpdateCheckResponse>('/system/update/check', {
        params: channel ? { channel } : undefined
      })
      return res.data
    })
  },
  startUpdate(payload: { channel: UpdateChannel; version: string; current_password: string }) {
    return callService(async () => {
      const res = await api.post<UpdateTransaction>('/system/update/jobs', payload)
      return res.data
    })
  },
  getUpdateJob(jobID: string) {
    return callService(async () => {
      const res = await api.get<UpdateTransaction>(`/system/update/jobs/${encodeURIComponent(jobID)}`)
      return res.data
    })
  }
}
