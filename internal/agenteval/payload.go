package agenteval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

// Tokens converts a UTF-8 byte length into a size ceiling. It tracks bytes.
// It is not a model tokenizer.
func Tokens(n int) int {
	return (n + 3) / 4
}

type ceiling struct {
	MaxBytes  int `json:"max_bytes"`
	MaxTokens int `json:"max_tokens"`
}

func baselinePath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "baseline.json"
	}
	return filepath.Join(filepath.Dir(file), "baseline.json")
}

func readBaseline(path string) (map[string]ceiling, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]ceiling{}, nil
	}
	if err != nil {
		return nil, err
	}
	var got map[string]ceiling
	if err := json.Unmarshal(data, &got); err != nil {
		return nil, err
	}
	if got == nil {
		got = map[string]ceiling{}
	}
	return got, nil
}

func writeBaseline(path string, ceilings map[string]ceiling) error {
	data, err := json.MarshalIndent(ceilings, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
