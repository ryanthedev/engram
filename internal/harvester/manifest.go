package harvester

import (
	"context"
	"errors"
	"fmt"

	yaml "go.yaml.in/yaml/v2"
)

var (
	// ErrUnknownCollection is returned when a collection is not registered on engram.
	ErrUnknownCollection = errors.New("harvester: unknown collection")
	// ErrUnknownSourceType is returned when a source type has no registered builder in the factory.
	ErrUnknownSourceType = errors.New("harvester: unknown source type")
	// ErrInvalidName is returned when a name fails the path-safe validation pattern.
	ErrInvalidName = errors.New("harvester: invalid name")
)

// Manifest defines the top-level harvester configuration.
type Manifest struct {
	Collections []CollectionManifest `yaml:"collections"`
}

// CollectionManifest defines configuration for a single knowledge collection.
type CollectionManifest struct {
	Name    string         `yaml:"name"`
	Sources []SourceConfig `yaml:"sources"`
}

// SourceConfig defines configuration for a single source.
type SourceConfig struct {
	Type string
	Raw  map[string]any
}

// UnmarshalYAML implements custom YAML unmarshaling for SourceConfig.
func (s *SourceConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var m map[string]any
	if err := unmarshal(&m); err != nil {
		var m2 map[interface{}]interface{}
		if err2 := unmarshal(&m2); err2 != nil {
			return err
		}
		m = cleanYAMLMap(m2)
	} else {
		m = cleanYAMLVal(m).(map[string]any)
	}

	tVal, ok := m["type"]
	if !ok {
		return errors.New("harvester: source config missing 'type' key")
	}
	tStr, ok := tVal.(string)
	if !ok {
		return fmt.Errorf("harvester: source config 'type' must be string, got %T", tVal)
	}

	s.Type = tStr
	s.Raw = m
	return nil
}

func cleanYAMLMap(m map[interface{}]interface{}) map[string]any {
	out := make(map[string]any)
	for k, v := range m {
		strKey, ok := k.(string)
		if !ok {
			strKey = fmt.Sprintf("%v", k)
		}
		out[strKey] = cleanYAMLVal(v)
	}
	return out
}

func cleanYAMLVal(v any) any {
	switch val := v.(type) {
	case map[interface{}]interface{}:
		return cleanYAMLMap(val)
	case map[string]any:
		out := make(map[string]any)
		for k, valVal := range val {
			out[k] = cleanYAMLVal(valVal)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = cleanYAMLVal(item)
		}
		return out
	default:
		return v
	}
}

// LoadManifest parses YAML manifest data into a Manifest.
func LoadManifest(data []byte) (Manifest, error) {
	if len(data) == 0 {
		return Manifest{}, fmt.Errorf("harvester: load manifest: empty YAML data")
	}

	var m Manifest
	if err := yaml.UnmarshalStrict(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("harvester: load manifest: %w", err)
	}

	seen := make(map[string]bool)
	for _, col := range m.Collections {
		if col.Name == "" {
			return Manifest{}, fmt.Errorf("harvester: load manifest: collection name is required")
		}
		if seen[col.Name] {
			return Manifest{}, fmt.Errorf("harvester: load manifest: duplicate collection name %q", col.Name)
		}
		seen[col.Name] = true
	}

	return m, nil
}

// InvalidNameError is returned when a collection or source type name is invalid.
type InvalidNameError struct {
	Name string
}

func (e *InvalidNameError) Error() string {
	return fmt.Sprintf("harvester: invalid name %q (must match ^[a-z0-9][a-z0-9_-]*$)", e.Name)
}

func (e *InvalidNameError) Unwrap() error {
	return ErrInvalidName
}

// UnknownCollectionError is returned when a collection is not registered.
type UnknownCollectionError struct {
	Name string
}

func (e *UnknownCollectionError) Error() string {
	return fmt.Sprintf("harvester: unknown collection %q", e.Name)
}

func (e *UnknownCollectionError) Unwrap() error {
	return ErrUnknownCollection
}

// UnknownSourceTypeError is returned when a source type is not registered.
type UnknownSourceTypeError struct {
	Type            string
	RegisteredTypes []string
}

func (e *UnknownSourceTypeError) Error() string {
	return fmt.Sprintf("harvester: unknown source type %q; registered types: %v", e.Type, e.RegisteredTypes)
}

func (e *UnknownSourceTypeError) Unwrap() error {
	return ErrUnknownSourceType
}

// Validate ensures the manifest's structural and registration constraints are met.
func (m Manifest) Validate(ctx context.Context, ec EngramClient) error {
	// 1. Path-safe check before network
	for _, col := range m.Collections {
		if err := validateName(col.Name); err != nil {
			return err
		}
		for _, src := range col.Sources {
			if err := validateName(src.Type); err != nil {
				return err
			}
		}
	}

	// 2. Registry registration check before network
	for _, col := range m.Collections {
		for _, src := range col.Sources {
			if !isSourceTypeRegistered(src.Type) {
				return &UnknownSourceTypeError{
					Type:            src.Type,
					RegisteredTypes: RegisteredTypes(),
				}
			}
		}
	}

	// 3. Live registration check (network)
	liveCols, err := ec.Collections(ctx)
	if err != nil {
		return fmt.Errorf("harvester: validating manifest: %w", err)
	}

	liveMap := make(map[string]bool)
	for _, c := range liveCols {
		liveMap[c.Name] = true
	}

	for _, col := range m.Collections {
		if !liveMap[col.Name] {
			return &UnknownCollectionError{Name: col.Name}
		}
	}

	return nil
}
