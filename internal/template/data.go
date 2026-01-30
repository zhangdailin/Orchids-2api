package template

// PageData represents the data passed to page templates
type PageData struct {
	Title     string
	AdminPath string
	ActiveTab string
	User      *UserInfo
	Stats     *Stats
	Config    *ConfigData
}

// UserInfo represents user information
type UserInfo struct {
	Username string
	Email    string
}

// Stats represents statistics data
type Stats struct {
	TotalAccounts    int
	NormalAccounts   int
	AbnormalAccounts int
	SelectedAccounts int
}

// ConfigData represents configuration data
type ConfigData struct {
	AdminPass            string
	AdminToken           string
	MaxRetries           int
	RetryDelay           int
	AccountSwitchCount   int
	RequestTimeout       int
	TokenRefreshInterval int
	AutoRefreshToken     bool
	AutoRefreshUsage     bool
}
