package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML can decode human strings like "30s"
// directly, since yaml.v3 has no native support for time.Duration.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Std() time.Duration {
	return time.Duration(d)
}
