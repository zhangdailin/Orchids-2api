package store

func BuildWarpSeedModels() []Model {
	return []Model{
		// Concrete IDs are retained only as a migration fallback; refresh replaces
		// them with the upstream account catalog. Synthetic warp-chat/warp-agent
		// models are intentionally not seeded.
		{Channel: "Warp", ModelID: "auto-open", Name: "Warp Auto Open", Status: ModelStatusAvailable, IsDefault: true, SortOrder: 0},
		{Channel: "Warp", ModelID: "gpt-5-2-low", Name: "GPT-5.2 Low (Warp)", Status: ModelStatusAvailable, SortOrder: 1},
		{Channel: "Warp", ModelID: "gpt-5-2-medium", Name: "GPT-5.2 Medium (Warp)", Status: ModelStatusAvailable, SortOrder: 2},
		{Channel: "Warp", ModelID: "gpt-5-2-high", Name: "GPT-5.2 High (Warp)", Status: ModelStatusAvailable, SortOrder: 3},
	}
}
