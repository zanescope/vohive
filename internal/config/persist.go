package config

func UpdateNotificationInFile(path string, telegram TelegramConfig, feishu FeishuConfig, qq QQConfig, webhook WebhookConfig, bark BarkConfig, email EmailConfig, pushplus PushplusConfig) error {
	return updateNotificationAST(path, telegram, feishu, qq, webhook, bark, email, pushplus)
}

// UpdateWebCredentialsInFile updates only the username and password keys while
// preserving unknown web settings and comments.
func UpdateWebCredentialsInFile(path string, username, password string) error {
	return updateWebCredentialsAST(path, username, password)
}
