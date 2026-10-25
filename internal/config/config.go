// Package config loads the site configuration (site.yaml, committed) and the
// credentials that go with it (.secrets, git-ignored). Every field has a
// default where one makes sense; unknown fields are errors, so a typo fails
// loudly instead of being silently ignored.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"go.yaml.in/yaml/v3"
)

// Default file names, relative to the working directory.
const (
	DefaultSitePath    = "site.yaml"
	DefaultSecretsPath = ".secrets"
)

// Site is site.yaml.
type Site struct {
	Switches []Switch `yaml:"switches"`
}

// Switch is a managed switch.
type Switch struct {
	Name    string `yaml:"name"`             // key into .secrets
	Driver  string `yaml:"driver,omitempty"` // default "gs316ep"
	Address string `yaml:"address"`          // host name or URL
}

// Drivers are the switch drivers stage0 knows.
var Drivers = []string{"gs316ep"}

// LoadSite reads and validates a site file.
func LoadSite(path string) (*Site, error) {
	var s Site
	if err := decode(path, &s); err != nil {
		return nil, err
	}
	if err := s.normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

func (s *Site) normalize() error {
	seen := map[string]bool{}
	for i := range s.Switches {
		sw := &s.Switches[i]
		switch {
		case sw.Name == "":
			return fmt.Errorf("switch %d: no name", i+1)
		case seen[sw.Name]:
			return fmt.Errorf("switch %q: defined twice", sw.Name)
		case sw.Address == "":
			return fmt.Errorf("switch %q: no address", sw.Name)
		}
		seen[sw.Name] = true
		if sw.Driver == "" {
			sw.Driver = Drivers[0]
		}
		if !slices.Contains(Drivers, sw.Driver) {
			return fmt.Errorf("switch %q: unknown driver %q (known: %v)", sw.Name, sw.Driver, Drivers)
		}
	}
	return nil
}

// Switch returns the named switch, or the only one when name is empty.
func (s *Site) Switch(name string) (Switch, error) {
	if name == "" {
		switch len(s.Switches) {
		case 0:
			return Switch{}, errors.New("no switches in site config")
		case 1:
			return s.Switches[0], nil
		default:
			return Switch{}, errors.New("several switches in site config; name one")
		}
	}
	for _, sw := range s.Switches {
		if sw.Name == name {
			return sw, nil
		}
	}
	return Switch{}, fmt.Errorf("no switch %q in site config", name)
}

// Secrets is .secrets.
type Secrets struct {
	Switches map[string]SwitchSecret `yaml:"switches"`
}

// SwitchSecret holds one switch's credentials: Password, or LoginHash, a
// precomputed login value from the older lab switch.py.
type SwitchSecret struct {
	Password  string `yaml:"password,omitempty"`
	LoginHash string `yaml:"loginHash,omitempty"`
}

// LoadSecrets reads the secrets file. A missing file means no secrets; a file
// that group or others can read is refused.
func LoadSecrets(path string) (*Secrets, error) {
	s := &Secrets{Switches: map[string]SwitchSecret{}}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by group or others; run: chmod 600 %s", path, path)
	}
	if err := decode(path, s); err != nil {
		return nil, err
	}
	return s, nil
}

func decode(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
