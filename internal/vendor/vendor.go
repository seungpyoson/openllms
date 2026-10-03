package vendor

import (
	"strings"

	"github.com/google/uuid"
)

// DefaultBaseURL returns the upstream OpenAI-compatible base for a vendor.
func DefaultBaseURL(vendor string) string {
	return DefaultBaseURLFor(vendor, "api_key")
}

// DefaultBaseURLFor picks upstream host. Codex/OpenAI oauth uses ChatGPT Codex backend, not api.openai.com.
func DefaultBaseURLFor(vendor, authType string) string {
	if strings.EqualFold(authType, "oauth") {
		switch strings.ToLower(vendor) {
		case "openai", "codex":
			return "https://chatgpt.com/backend-api/codex"
		}
	}
	switch strings.ToLower(vendor) {
	case "deepseek":
		return "https://api.deepseek.com/v1"
	case "openai", "codex":
		return "https://api.openai.com/v1"
	case "claude", "anthropic":
		return "https://api.anthropic.com"
	case "kimi", "moonshot":
		return "https://api.moonshot.cn/v1"
	case "glm":
		return "https://open.bigmodel.cn/api/paas/v4"
	default:
		return ""
	}
}

// CredentialPath uses generated identities rather than user-controlled names.
func CredentialPath(projectID, credentialID uuid.UUID) string {
	return "/llms/" + projectID.String() + "/credentials/" + credentialID.String()
}

const SecretName = "credential"
