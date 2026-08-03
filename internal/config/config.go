// Package config loads and validates the daemon configuration from a YAML file.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level daemon configuration.
type Config struct {
	Matrix Matrix `yaml:"matrix"`
	Poll   Poll   `yaml:"poll"`
	DBPath string `yaml:"db_path"`
}

// Matrix holds Matrix homeserver connection details.
type Matrix struct {
	Homeserver  string `yaml:"homeserver"`
	UserID      string `yaml:"user_id"`
	AccessToken string `yaml:"access_token"`
	RoomID      string `yaml:"room_id"`
}

// Poll holds polling behaviour settings.
type Poll struct {
	IntervalSeconds int `yaml:"interval_seconds"`
	MaxPages        int `yaml:"max_pages"`
	// JitterPercent spreads each wait randomly by up to ±this percentage of
	// the interval, so polls do not land on a fixed, obviously automated
	// schedule. Set to 0 to poll on an exact interval.
	JitterPercent *int `yaml:"jitter_percent"`
}

// Load reads, parses and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Poll.IntervalSeconds == 0 {
		c.Poll.IntervalSeconds = 300
	}
	if c.Poll.MaxPages == 0 {
		c.Poll.MaxPages = 10
	}
	if c.Poll.JitterPercent == nil {
		d := 20
		c.Poll.JitterPercent = &d
	}
	if c.DBPath == "" {
		c.DBPath = "./standvirtual.db"
	}
}

func (c *Config) validate() error {
	switch {
	case c.Matrix.Homeserver == "":
		return fmt.Errorf("matrix.homeserver is required")
	case c.Matrix.UserID == "":
		return fmt.Errorf("matrix.user_id is required")
	case c.Matrix.AccessToken == "":
		return fmt.Errorf("matrix.access_token is required")
	case c.Matrix.RoomID == "":
		return fmt.Errorf("matrix.room_id is required")
	case c.Poll.IntervalSeconds < 30:
		return fmt.Errorf("poll.interval_seconds must be >= 30 to avoid hammering Standvirtual")
	case *c.Poll.JitterPercent < 0 || *c.Poll.JitterPercent > 50:
		return fmt.Errorf("poll.jitter_percent must be between 0 and 50")
	}
	return nil
}
