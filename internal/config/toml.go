package config

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/pelletier/go-toml/v2"
)

// defaultTOML uses the same representation as JSON, including the model
// entry, so the generated file and hand-written files share one schema.
func defaultTOML() ([]byte, error) {
	data, err := json.Marshal(Default())
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var values map[string]any
	if err := decoder.Decode(&values); err != nil {
		return nil, err
	}
	if err := tomlIntegers(values); err != nil {
		return nil, err
	}
	return toml.Marshal(values)
}

// The configuration's numeric fields are all integers. Keep them integers
// rather than letting JSON's generic decoder turn them into TOML floats.
func tomlIntegers(value any) error {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if number, ok := child.(json.Number); ok {
				n, err := number.Int64()
				if err != nil {
					return fmt.Errorf("encode TOML: %w", err)
				}
				value[key] = n
			} else if err := tomlIntegers(child); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range value {
			if number, ok := child.(json.Number); ok {
				n, err := number.Int64()
				if err != nil {
					return fmt.Errorf("encode TOML: %w", err)
				}
				value[i] = n
			} else if err := tomlIntegers(child); err != nil {
				return err
			}
		}
	}
	return nil
}
