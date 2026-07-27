package notify

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zanescope/vohive/internal/db"
)

type commandSIMEntry struct {
	DeviceID string
	ICCID    string
	Phone    string
	Operator string
	Note     string
	ESIM     bool
	Enabled  bool
	Current  bool
}

func (m *Manager) handleCmdHelp(_ CommandContext, _ []string) string {
	return strings.Join([]string{
		"支持的指令",
		"",
		"SIM 与短信",
		"/sim [设备ID]  列出可用 SIM/eSIM 卡",
		"/send [设备ID] [手机号] [短信内容]  发送短信",
		"/sms [设备ID]  查看最近短信",
		"",
		"设备与网络",
		"/list  查看设备总览",
		"/status [设备ID]  查看设备详情",
		"/rotate [设备ID]  切换公网 IP",
		"",
		"eSIM 与通话",
		"/esim [设备ID]  查看 eSIM Profile",
		"/switch [设备ID] [序号或ICCID]  切换 eSIM",
		"/vocall [设备ID] [接收号码] [保持秒数]  发起 VoWiFi 呼叫",
		"",
		"群聊提示：飞书使用 @机器人 /指令；Telegram 使用 /指令 或 /指令@Bot用户名；QQ 使用 /指令。",
	}, "\n")
}

func (m *Manager) handleCmdSIM(_ CommandContext, args []string) string {
	if len(args) > 1 {
		return commandUsageBlock("SIM/eSIM 列表", "/sim [设备ID]", "/sim wwan9")
	}
	if m == nil || m.pool == nil {
		return commandEmptyBlock("SIM/eSIM 列表", "没有可用设备")
	}

	workers := m.pool.GetAllWorkers()
	if len(args) == 1 {
		deviceID := strings.TrimSpace(args[0])
		worker := m.pool.GetWorker(deviceID)
		if worker == nil {
			return commandFailureBlock("SIM/eSIM 列表", deviceID, "设备未找到")
		}
		workers = workers[:0]
		workers = append(workers, worker)
	}

	entries := make([]commandSIMEntry, 0)
	for _, worker := range workers {
		if worker == nil {
			continue
		}
		status := worker.GetCachedDeviceStatus()
		currentICCID := strings.TrimSpace(status.ICCID)
		profilesByICCID := make(map[string]commandSIMEntry)
		if worker.EsimMgr != nil {
			if groups, err := worker.EsimMgr.GetProfiles(); err == nil {
				for _, group := range groups {
					for _, profile := range group.Profiles {
						iccid := strings.TrimSpace(profile.ICCID)
						if iccid == "" {
							continue
						}
						current := iccid == currentICCID
						entry := commandSIMEntry{
							DeviceID: worker.ID,
							ICCID:    iccid,
							Operator: strings.TrimSpace(profile.ServiceProviderName),
							ESIM:     true,
							Enabled:  profile.State == 1 || current,
							Current:  current,
						}
						profilesByICCID[iccid] = entry
					}
				}
			}
		}

		if currentICCID != "" {
			if _, isESIM := profilesByICCID[currentICCID]; !isESIM {
				entries = append(entries, commandSIMEntry{
					DeviceID: worker.ID,
					ICCID:    currentICCID,
					Operator: firstNonEmpty(status.NativeSPN, status.Operator),
					Enabled:  true,
					Current:  true,
				})
			}
		}
		for _, entry := range profilesByICCID {
			if entry.Current && entry.Operator == "" {
				entry.Operator = firstNonEmpty(status.NativeSPN, status.Operator)
			}
			entries = append(entries, entry)
		}
	}

	for index := range entries {
		entry := &entries[index]
		imsi := ""
		if entry.Current {
			if worker := m.pool.GetWorker(entry.DeviceID); worker != nil {
				status := worker.GetCachedDeviceStatus()
				imsi = strings.TrimSpace(status.IMSI)
			}
		}
		if phone, err := db.GetPhoneNumberByIMSIOrICCID(imsi, entry.ICCID); err == nil {
			entry.Phone = strings.TrimSpace(phone)
		}
		if note, err := db.GetSIMCardNote(entry.ICCID); err == nil {
			entry.Note = strings.TrimSpace(note)
		}
		if entry.Operator == "" && db.DB != nil {
			var card db.SIMCard
			if err := db.DB.Select("operator").Where("iccid = ?", entry.ICCID).First(&card).Error; err == nil {
				entry.Operator = strings.TrimSpace(card.Operator)
			}
		}
	}

	if len(entries) == 0 {
		return commandEmptyBlock("SIM/eSIM 列表", "没有检测到可用 SIM/eSIM 卡")
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].DeviceID != entries[j].DeviceID {
			return entries[i].DeviceID < entries[j].DeviceID
		}
		if entries[i].Current != entries[j].Current {
			return entries[i].Current
		}
		if entries[i].ESIM != entries[j].ESIM {
			return !entries[i].ESIM
		}
		return entries[i].ICCID < entries[j].ICCID
	})
	return formatCommandSIMEntries(entries)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" && !strings.EqualFold(value, "unknown") {
			return value
		}
	}
	return ""
}

func formatCommandSIMEntries(entries []commandSIMEntry) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("SIM/eSIM 列表（%d）\n\n", len(entries)))
	for index, entry := range entries {
		kind := "SIM"
		if entry.ESIM {
			state := "未启用"
			if entry.Enabled {
				state = "启用"
			}
			kind = "eSIM（" + state + "）"
		}
		builder.WriteString(fmt.Sprintf("%d. 设备  %s\n", index+1, entry.DeviceID))
		builder.WriteString(fmt.Sprintf("类型    %s\n", kind))
		builder.WriteString(fmt.Sprintf("ICCID   %s\n", maskedCommandICCID(entry.ICCID)))
		builder.WriteString(fmt.Sprintf("本机    %s\n", commandValueOrDash(entry.Phone)))
		builder.WriteString(fmt.Sprintf("运营商  %s\n", commandValueOrDash(entry.Operator)))
		builder.WriteString(fmt.Sprintf("备注    %s", commandValueOrDash(entry.Note)))
		if index+1 < len(entries) {
			builder.WriteString("\n\n")
		}
	}
	return builder.String()
}

func commandValueOrDash(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "--"
}

func maskedCommandICCID(iccid string) string {
	iccid = strings.TrimSpace(iccid)
	if len(iccid) <= 4 {
		return commandValueOrDash(iccid)
	}
	return "****" + iccid[len(iccid)-4:]
}
