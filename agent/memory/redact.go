package memory

import (
	"encoding/json"
	"regexp"
	"strings"
)

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\b(?:ghp_|gho_|ghu_|ghs_|github_pat_)[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bAKIA[A-Z0-9]{16}\b`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]{8,}={0,2}`),
	regexp.MustCompile(`(?i)(?:api[_ -]?key|access[_ -]?token|client[_ -]?secret|password|private[_ -]?key)["']?\s*[:=]\s*["']?[^\s"',;}{]{8,}`),
	regexp.MustCompile(`(?s)-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----.*?-----END (?:RSA |EC |OPENSSH )?PRIVATE KEY-----`),
}

func redact(text string) string {
	for _, pattern := range credentialPatterns {
		text = pattern.ReplaceAllString(text, "[credential redacted]")
	}
	return text
}

func sanitizeJSON(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	var clean func(any) any
	clean = func(v any) any {
		switch typed := v.(type) {
		case string:
			var nested any
			if (strings.HasPrefix(strings.TrimSpace(typed), "{") || strings.HasPrefix(strings.TrimSpace(typed), "[")) && json.Unmarshal([]byte(typed), &nested) == nil {
				encoded, _ := json.Marshal(clean(nested))
				return string(encoded)
			}
			return redact(typed)
		case []any:
			for i, item := range typed {
				typed[i] = clean(item)
			}
			return typed
		case map[string]any:
			for k, item := range typed {
				normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(k, "_", ""), "-", ""))
				switch normalized {
				case "apikey", "authorization", "password", "accesstoken", "refreshtoken", "clientsecret", "privatekey":
					typed[k] = "[credential redacted]"
				default:
					typed[k] = clean(item)
				}
			}
			return typed
		default:
			return v
		}
	}
	return json.Marshal(clean(value))
}
