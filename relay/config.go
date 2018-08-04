package relay

import (
	"encoding/json"

	"github.com/ngaut/log"
)

// Config is the configuration for Relay.
type Config struct {
	EnableGTID     bool     `toml:"enable-gtid" json:"enable-gtid"`
	AutoFixGTID    bool     `toml:"auto-fix-gtid" json:"auto-fix-gtid"`
	MetaFile       string   `toml:"meta-file" json:"meta-file"`
	RelayDir       string   `toml:"relay-dir" json:"relay-dir"`
	ServerID       int      `toml:"server-id" json:"server-id"`
	Flavor         string   `toml:"flavor" json:"flavor"`
	Charset        string   `toml:"charset" json:"charset"`
	VerifyChecksum bool     `toml:"verify-checksum" json:"verify-checksum"`
	From           DBConfig `toml:"data-source" json:"data-source"`
}

// DBConfig is the DB configuration.
type DBConfig struct {
	Host     string `toml:"host" json:"host"`
	User     string `toml:"user" json:"user"`
	Password string `toml:"password" json:"-"` // omit it for privacy
	Port     int    `toml:"port" json:"port"`
}

func (c *Config) String() string {
	cfg, err := json.Marshal(c)
	if err != nil {
		log.Errorf("[relay] marshal config to json error %v", err)
	}
	return string(cfg)
}
