package config

import (
	"encoding/json"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so config files can carry human-readable
// values ("30s", "2m") instead of raw nanosecond integers, which is what
// encoding/json and yaml.v3 would otherwise demand.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML accepts either a duration string ("30s") or a bare integer,
// which is interpreted as seconds.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	// yaml.v3 will happily decode the scalar `45` into the string "45", so the
	// numeric case has to be tried on ParseDuration *failure* rather than on
	// string-decode failure — otherwise the bare-seconds form is unreachable.
	var s string
	if err := value.Decode(&s); err == nil {
		if parsed, perr := time.ParseDuration(s); perr == nil {
			*d = Duration(parsed)
			return nil
		}

		var secs int64
		if nerr := value.Decode(&secs); nerr == nil {
			*d = Duration(time.Duration(secs) * time.Second)
			return nil
		}
		return fmt.Errorf("invalid duration %q: want a string like \"30s\" or a number of seconds", s)
	}

	var secs int64
	if err := value.Decode(&secs); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\" or a number of seconds, got %q", value.Value)
	}
	*d = Duration(time.Duration(secs) * time.Second)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}
