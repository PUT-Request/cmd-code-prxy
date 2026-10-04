package config

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// writeMu serializes read-modify-write cycles on the config file so two
// concurrent dashboard saves cannot clobber each other.
var writeMu sync.Mutex

// UpdateAPIKeys rewrites only the api_keys section of the config file, leaving
// every other key (including comments' surrounding structure) intact. The write
// is atomic: a temp file in the same directory followed by a rename.
func UpdateAPIKeys(path string, defs []APIKeyDef) error {
	writeMu.Lock()
	defer writeMu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	// Decode into a yaml.Node so untouched sections keep their original keys
	// and ordering instead of being re-serialized in Go struct order.
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("failed to parse config file %s: %w", path, err)
	}
	if len(root.Content) == 0 {
		return fmt.Errorf("config file %s is empty", path)
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return fmt.Errorf("config file %s is not a mapping", path)
	}

	var keysNode yaml.Node
	if err := keysNode.Encode(defs); err != nil {
		return fmt.Errorf("failed to encode api_keys: %w", err)
	}

	replaced := false
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "api_keys" {
			doc.Content[i+1] = &keysNode
			replaced = true
			break
		}
	}
	if !replaced {
		doc.Content = append(doc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "api_keys"},
			&keysNode,
		)
	}

	out, err := marshalPreservingHeader(&root, data)
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("failed to write temp config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("failed to replace config: %w", err)
	}
	return nil
}

// marshalPreservingHeader keeps a leading comment block (or #! line) at the top
// of the file, since yaml.Node round-trips body content but not a detached
// document preamble.
func marshalPreservingHeader(node *yaml.Node, original []byte) ([]byte, error) {
	body, err := yaml.Marshal(node)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize config: %w", err)
	}
	header := leadingHeader(original)
	if header == "" {
		return body, nil
	}
	if len(body) > 0 && body[0] == '#' {
		return body, nil
	}
	return append([]byte(header), body...), nil
}

// leadingHeader returns the leading comment/shebang lines of a YAML file.
func leadingHeader(data []byte) string {
	text := string(data)
	lines := strings.Split(text, "\n")
	var head []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if len(head) == 0 {
				continue
			}
			break
		}
		if strings.HasPrefix(trimmed, "#") {
			head = append(head, line)
			continue
		}
		break
	}
	if len(head) == 0 {
		return ""
	}
	return strings.Join(head, "\n") + "\n"
}
