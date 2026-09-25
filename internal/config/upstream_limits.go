package config

import "time"

func (c *Config) WarpStreamIdleTimeout() time.Duration {
	value := 0
	if c != nil {
		value = c.WarpStreamIdleSeconds
	}
	return time.Duration(boundedDefault(value, 300, 3600)) * time.Second
}
