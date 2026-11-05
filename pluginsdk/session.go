package pluginsdk

import (
	"crypto/rand"
	"encoding/hex"
)

// Instance and epoch headers: the plugin sends them on host requests, and
// fleet sends them on API calls to name the authority it admitted them under.
const (
	InstanceHeader = "X-Fleet-Plugin-Instance"
	EpochHeader    = "X-Fleet-Plugin-Epoch"
)

// SessionPath is the host session route.
const SessionPath = "/fleet-host/v3/session"

// APIPath prefixes the opaque public API routes the plugin serves.
const APIPath = "/fleet-plugin/v3/api/"

// RandomID returns 32 random lowercase hex characters, for instance IDs.
func RandomID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic("system random source unavailable")
	}
	return hex.EncodeToString(data[:])
}

// ValidUID accepts 32 lowercase hex characters: instance IDs, epochs, agent
// and group IDs.
func ValidUID(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == 16 && hex.EncodeToString(b) == value
}

// Session is the plugin's current authority. The epoch changes on every
// lifecycle change, plugin restart and loss of readiness; work admitted under
// an older epoch must stop.
type Session struct {
	PluginID    string   `json:"plugin_id"`
	InstanceID  string   `json:"instance_id"`
	Epoch       string   `json:"epoch"`
	Revision    int64    `json:"revision"`
	Permissions []string `json:"permissions"`
}
