package config

import (
	"fmt"

	yaml "go.yaml.in/yaml/v3"
)

func updateNotificationAST(path string, telegram TelegramConfig, feishu FeishuConfig, qq QQConfig, webhook WebhookConfig, bark BarkConfig, email EmailConfig, pushplus PushplusConfig) error {
	return patchConfigFile(path, func(root *yaml.Node) error {
		sections := []struct {
			name   string
			fields []yamlFieldPatch
		}{
			{"telegram", []yamlFieldPatch{
				{"enabled", telegram.Enabled}, {"bot_token", telegram.BotToken},
				{"chat_id", telegram.ChatID}, {"admin_id", telegram.AdminID},
				{"base_url", telegram.BaseURL}, {"proxy", telegram.Proxy},
			}},
			{"feishu", []yamlFieldPatch{
				{"enabled", feishu.Enabled}, {"app_id", feishu.AppID},
				{"app_secret", feishu.AppSecret}, {"chat_ids", feishu.ChatIDs},
			}},
			{"qq", []yamlFieldPatch{
				{"enabled", qq.Enabled}, {"app_id", qq.AppID},
				{"app_secret", qq.AppSecret}, {"group_ids", qq.GroupIDs},
				{"direct_ids", qq.DirectIDs},
			}},
			{"webhook", []yamlFieldPatch{
				{"enabled", webhook.Enabled}, {"urls", webhook.URLs},
				{"secret", webhook.Secret}, {"timeout_ms", webhook.TimeoutMs},
				{"retry_max", webhook.RetryMax}, {"text_template", webhook.TextTemplate},
				{"headers", webhook.Headers},
			}},
			{"bark", []yamlFieldPatch{
				{"enabled", bark.Enabled}, {"urls", bark.URLs}, {"group", bark.Group},
				{"icon", bark.Icon}, {"level", bark.Level},
			}},
			{"email", []yamlFieldPatch{
				{"enabled", email.Enabled}, {"use_ssl", email.UseSSL},
				{"smtp_host", email.SMTPHost}, {"smtp_port", email.SMTPPort},
				{"username", email.Username}, {"password", email.Password},
				{"from_address", email.FromAddress}, {"to_addresses", email.ToAddresses},
			}},
			{"pushplus", []yamlFieldPatch{
				{"enabled", pushplus.Enabled}, {"token", pushplus.Token},
				{"topic", pushplus.Topic}, {"channel", pushplus.Channel},
			}},
		}
		for _, section := range sections {
			if err := patchMapping(root, section.name, section.fields...); err != nil {
				return err
			}
		}
		return nil
	})
}

func updateWebCredentialsAST(path, username, password string) error {
	return patchConfigFile(path, func(root *yaml.Node) error {
		return patchMapping(root, "web",
			yamlFieldPatch{"username", username},
			yamlFieldPatch{"password", password},
		)
	})
}

func updateDevicesAST(path string, mutate func(*yaml.Node) (*yaml.Node, error)) error {
	err := patchConfigFile(path, func(root *yaml.Node) error {
		devices := getMapValue(root, "devices")
		if devices == nil || devices.Kind != yaml.SequenceNode {
			devices = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			setMapNode(root, "devices", devices)
		}
		_, err := mutate(devices)
		return err
	})
	if err != nil {
		return err
	}
	_ = ReloadFromFile()
	return nil
}

func updateProxyAST(path string, mutate func(*yaml.Node) error) error {
	err := patchConfigFile(path, func(root *yaml.Node) error {
		proxy := ensureMapping(root, "proxy")
		if err := mutate(proxy); err != nil {
			return fmt.Errorf("update proxy config: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	_ = ReloadFromFile()
	return nil
}
