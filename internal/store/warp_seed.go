package store

func BuildWarpSeedModels() []Model {
	return []Model{
		{Channel: "Warp", ModelID: "auto-open", Name: "Warp Auto Open", Status: ModelStatusAvailable, IsDefault: true, SortOrder: 0},
		{Channel: "Warp", ModelID: "gpt-5-2-low", Name: "GPT-5.2 Low (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 1},
		{Channel: "Warp", ModelID: "gpt-5-2-medium", Name: "GPT-5.2 Medium (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 2},
		{Channel: "Warp", ModelID: "gpt-5-2-high", Name: "GPT-5.2 High (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 3},
	}
}
