package config

import (
	"crypto/sha256"
	"fmt"

	"go.yaml.in/yaml/v3"
)

// Digest fingerprints the decoded configuration rather than file formatting.
func Digest(cfg Config) (string, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}
