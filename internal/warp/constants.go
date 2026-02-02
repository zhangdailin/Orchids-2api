package warp

const (
	refreshURL = "https://app.warp.dev/proxy/token?key=AIzaSyBdy3O3S9hrdayLJxJ7mriBR4qgUaUygAs"
	loginURL   = "https://app.warp.dev/client/login"
	aiURL      = "https://app.warp.dev/ai/multi-agent"

	clientID      = "warp-app"
	clientVersion = "v0.2026.01.14.08.15.stable_04"
	osCategory    = "macOS"
	osName        = "macOS"
	osVersion     = "26.3"
	userAgent     = "Warp/" + clientVersion
	identifier    = "cli-agent-auto"
)

const defaultModel = "auto"

const noWarpToolsPrompt = `IMPORTANT INSTRUCTIONS:
- Do NOT use Warp's built-in tools (like terminal commands, file operations, etc.)
- ONLY use the tools explicitly provided by the client through tool calls
- If you need to perform an action, use the available client tools
- Available client tools will be listed in the tool definitions`

// Built-in refresh token payload (base64 decoded) used when no account refresh token is provided.
const refreshTokenB64 = "Z3JhbnRfdHlwZT1yZWZyZXNoX3Rva2VuJnJlZnJlc2hfdG9rZW49QU1mLXZCeFNSbWRodmVHR0JZTTY5cDA1a0RoSW4xaTd3c2NBTEVtQzlmWURScEh6akVSOWRMN2trLWtIUFl3dlk5Uk9rbXk1MHFHVGNBb0JaNEFtODZoUFhrcFZQTDkwSEptQWY1Zlo3UGVqeXBkYmNLNHdzbzhLZjNheGlTV3RJUk9oT2NuOU56R2FTdmw3V3FSTU5PcEhHZ0JyWW40SThrclc1N1I4X3dzOHU3WGNTdzh1MERpTDlIcnBNbTBMdHdzQ2g4MWtfNmJiMkNXT0ViMWxJeDNIV1NCVGVQRldzUQ=="
