package relay

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentparley/tunnel/internal/config"
	"github.com/agentparley/tunnel/internal/shellrun"
	"gopkg.in/yaml.v3"
)

// Allow adds a host:port to relay.allow. It reports whether the file changed.
func Allow(path, entry string) (bool, error) {
	canonical, err := config.CanonicalAllowEntry(entry)
	if err != nil {
		return false, err
	}
	return edit(path, func(relay *yaml.Node) (bool, error) {
		list := childSequence(relay, "allow")
		for _, item := range list.Content {
			if existing, err := config.CanonicalAllowEntry(item.Value); err == nil && existing == canonical {
				return false, nil
			}
		}
		list.Content = append(list.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: canonical})
		return true, nil
	})
}

// Disallow removes a host:port from relay.allow. It reports whether the file changed.
func Disallow(path, entry string) (bool, error) {
	canonical, err := config.CanonicalAllowEntry(entry)
	if err != nil {
		return false, err
	}
	return edit(path, func(relay *yaml.Node) (bool, error) {
		list := childSequence(relay, "allow")
		kept := list.Content[:0:0]
		for _, item := range list.Content {
			if existing, err := config.CanonicalAllowEntry(item.Value); err == nil && existing == canonical {
				continue
			}
			kept = append(kept, item)
		}
		changed := len(kept) != len(list.Content)
		list.Content = kept
		return changed, nil
	})
}

// AddProgram saves (or replaces) a named program in relay.programs.
func AddProgram(path, name string, program config.RelayProgram) (bool, error) {
	if err := config.ValidateRelayProgram(name, program); err != nil {
		return false, err
	}
	var encoded yaml.Node
	if err := encoded.Encode(program); err != nil {
		return false, err
	}
	return edit(path, func(relay *yaml.Node) (bool, error) {
		programs := childMapping(relay, "programs")
		for index := 0; index+1 < len(programs.Content); index += 2 {
			if programs.Content[index].Value == name {
				var before, after bytes.Buffer
				_ = yaml.NewEncoder(&before).Encode(programs.Content[index+1])
				_ = yaml.NewEncoder(&after).Encode(&encoded)
				if before.String() == after.String() {
					return false, nil
				}
				programs.Content[index+1] = &encoded
				return true, nil
			}
		}
		programs.Content = append(programs.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, &encoded)
		return true, nil
	})
}

// RemoveProgram deletes a named program from relay.programs.
func RemoveProgram(path, name string) (bool, error) {
	if err := config.ValidateProgramName(name); err != nil {
		return false, err
	}
	return edit(path, func(relay *yaml.Node) (bool, error) {
		programs := childMapping(relay, "programs")
		for index := 0; index+1 < len(programs.Content); index += 2 {
			if programs.Content[index].Value == name {
				programs.Content = append(programs.Content[:index], programs.Content[index+2:]...)
				return true, nil
			}
		}
		return false, nil
	})
}

// edit round-trips config.yaml through yaml.Node so comments and unrelated keys survive.
func edit(path string, change func(relay *yaml.Node) (bool, error)) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("reading config %s: %w", path, err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return false, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if document.Kind == 0 {
		document = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return false, fmt.Errorf("config %s is not a YAML mapping", path)
	}

	changed, err := change(childMapping(root, "relay"))
	if err != nil || !changed {
		return false, err
	}

	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return false, err
	}
	if err := encoder.Close(); err != nil {
		return false, err
	}

	var check config.Config
	if err := yaml.Unmarshal(output.Bytes(), &check); err != nil {
		return false, fmt.Errorf("the edited config would not parse: %w", err)
	}
	if err := check.Relay.Validate(); err != nil {
		return false, err
	}
	return true, writeConfig(path, output.Bytes(), check.RunAs)
}

// writeConfig replaces the file atomically as 0640, owned by root with the run_as user's group, because the daemon
// (running as that user) must read it and relay env values may be secrets.
func writeConfig(path string, data []byte, runAs string) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempPath, 0o640); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		group := -1
		if user, err := shellrun.ResolveUser(runAs); err == nil {
			group = user.GID
		}
		if err := os.Chown(tempPath, 0, group); err != nil {
			return err
		}
	}
	return os.Rename(tempPath, path)
}

func childMapping(parent *yaml.Node, key string) *yaml.Node {
	return child(parent, key, yaml.MappingNode, "!!map")
}

func childSequence(parent *yaml.Node, key string) *yaml.Node {
	return child(parent, key, yaml.SequenceNode, "!!seq")
}

func child(parent *yaml.Node, key string, kind yaml.Kind, tag string) *yaml.Node {
	for index := 0; index+1 < len(parent.Content); index += 2 {
		if parent.Content[index].Value == key {
			value := parent.Content[index+1]
			if value.Kind != kind {
				value.Kind, value.Tag, value.Value, value.Content, value.Style = kind, tag, "", nil, 0
			}
			return value
		}
	}
	value := &yaml.Node{Kind: kind, Tag: tag}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
	return value
}
