package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// WebhookClient は通知を送信するためのクライアントです
type WebhookClient struct {
	URL string
}

// JobNotification はJobの通知情報を表します
type JobNotification struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Status    string `json:"status"`
	Message   string `json:"message"`
}

// DiscordEmbed はDiscord埋め込みメッセージを表します
type DiscordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Color       int            `json:"color"`
	Fields      []DiscordField `json:"fields,omitempty"`
}

// DiscordField はDiscord埋め込みメッセージのフィールドを表します
type DiscordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// DiscordPayload はDiscord webhookに送信するペイロードを表します
type DiscordPayload struct {
	Username  string         `json:"username,omitempty"`
	Embeds    []DiscordEmbed `json:"embeds"`
}

// NewWebhookClient は新しいWebhookClientを作成します
func NewWebhookClient(url string) *WebhookClient {
	return &WebhookClient{
		URL: url,
	}
}

// SendNotification はWebhookに通知を送信します
func (c *WebhookClient) SendNotification(notification JobNotification) error {
	// URLがDiscordのWebhook URLかどうかを判断
	if strings.Contains(c.URL, "discord.com/api/webhooks") {
		return c.sendDiscordNotification(notification)
	}
	
	// 一般的なWebhook通知（既存のコード）
	payload, err := json.Marshal(notification)
	if (err != nil) {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}

	resp, err := http.Post(c.URL, "application/json", bytes.NewBuffer(payload))
	if (err != nil) {
		return fmt.Errorf("failed to send webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned non-success status: %d", resp.StatusCode)
	}

	return nil
}

// sendDiscordNotification はDiscord形式のWebhook通知を送信します
func (c *WebhookClient) sendDiscordNotification(notification JobNotification) error {
	var color int
	
	// ステータスに基づいて埋め込みカラーを設定
	switch notification.Status {
	case "Completed":
		color = 5763719  // 緑色
	case "Failed":
		color = 15548997 // 赤色
	default:
		color = 16776960 // 黄色
	}
	
	// Discord用のペイロードを作成
	payload := DiscordPayload{
		Username: "Kubernetes Job Monitor",
		Embeds: []DiscordEmbed{
			{
				Title:       fmt.Sprintf("Job %s - %s", notification.Name, notification.Status),
				Description: notification.Message,
				Color:       color,
				Fields: []DiscordField{
					{
						Name:   "Job",
						Value:  notification.Name,
						Inline: true,
					},
					{
						Name:   "Namespace",
						Value:  notification.Namespace,
						Inline: true,
					},
					{
						Name:   "Status",
						Value:  notification.Status,
						Inline: true,
					},
				},
			},
		},
	}
	
	// JSONにエンコード
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal Discord payload: %w", err)
	}

	// POSTリクエストを送信
	resp, err := http.Post(c.URL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to send Discord webhook: %w", err)
	}
	defer resp.Body.Close()

	// レスポンスをチェック
	if resp.StatusCode >= 300 {
		return fmt.Errorf("Discord webhook returned non-success status: %d", resp.StatusCode)
	}

	return nil
}
