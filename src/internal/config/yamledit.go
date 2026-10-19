package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// EditFile applies edit to the YAML configuration file at path and writes
// it back atomically (temporary file + rename), keeping the previous
// content as path+".bak". Only the keys edit touches change: every other
// section, key and comment is preserved. A missing file starts empty. The
// result must load (Load) or the original file is left untouched.
func EditFile(path string, edit func(root *yaml.Node) error) error {
	if path == "" {
		return errors.New("config: no configuration file (set OVERSEER_CONFIG_PATH)")
	}
	orig, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("config: reading %s: %w", path, err)
	}
	var doc yaml.Node
	if len(bytes.TrimSpace(orig)) > 0 {
		if err := yaml.Unmarshal(orig, &doc); err != nil {
			return fmt.Errorf("config: parsing %s: %w", path, err)
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("config: %s is not a YAML mapping", path)
	}
	if err := edit(doc.Content[0]); err != nil {
		return err
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("config: encoding %s: %w", path, err)
	}
	if err := enc.Close(); err != nil {
		return err
	}
	// The edited file must still be a valid configuration.
	var probe Config
	if err := yaml.Unmarshal(out.Bytes(), &probe); err != nil {
		return fmt.Errorf("config: edited %s does not parse: %w", path, err)
	}
	mode := fs.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if len(orig) > 0 {
		if err := os.WriteFile(path+".bak", orig, mode); err != nil {
			return fmt.Errorf("config: backup of %s: %w", path, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// MappingAt returns the mapping at the key path under m, creating empty
// mappings along the way.
func MappingAt(m *yaml.Node, keys ...string) *yaml.Node {
	for _, k := range keys {
		v := valueOf(m, k)
		if v == nil || v.Kind != yaml.MappingNode {
			nv := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setValue(m, k, nv)
			v = nv
		}
		m = v
	}
	return m
}

// SetString sets key to a string value, or removes it when value is empty
// (the default applies).
func SetString(m *yaml.Node, key, value string) {
	if value == "" {
		DeleteKey(m, key)
		return
	}
	setValue(m, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

// SetInt sets key to an integer, or removes it when value is zero.
func SetInt(m *yaml.Node, key string, value int64) {
	if value == 0 {
		DeleteKey(m, key)
		return
	}
	setValue(m, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(value, 10)})
}

// SetStrings sets key to a list of strings, or removes it when empty.
func SetStrings(m *yaml.Node, key string, values []string) {
	if len(values) == 0 {
		DeleteKey(m, key)
		return
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	if len(values) > 3 {
		seq.Style = 0
	}
	for _, v := range values {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
	}
	setValue(m, key, seq)
}

// DeleteKey removes key from mapping m (no-op when absent).
func DeleteKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func valueOf(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setValue replaces key's value (keeping the key's comments) or appends
// the key.
func setValue(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			old := m.Content[i+1]
			v.LineComment, v.FootComment = old.LineComment, old.FootComment
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}

// LoadFileOnly reads the configuration file over the defaults without the
// environment overrides or validation: what the file itself says.
func LoadFileOnly(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("config: reading %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	return cfg, nil
}

// DatabaseEnvOverrides maps each database setting (its YAML key) to the
// OVERSEER_* environment variable that overrides it, for the variables
// that are set.
func DatabaseEnvOverrides() map[string]string {
	keys := map[string]string{"host": "OVERSEER_DATABASE_HOST", "port": "OVERSEER_DATABASE_PORT", "name": "OVERSEER_DATABASE_NAME",
		"user": "OVERSEER_DATABASE_USER", "password": "OVERSEER_DATABASE_PASSWORD", "ssl_mode": "OVERSEER_DATABASE_SSL_MODE",
		"max_conns": "OVERSEER_DATABASE_MAX_CONNS", "application_name": "OVERSEER_DATABASE_APPLICATION_NAME"}
	out := map[string]string{}
	for k, env := range keys {
		if v, ok := os.LookupEnv(env); ok && v != "" {
			out[k] = env
		}
	}
	return out
}
