package postgres

import (
	"bytes"
	"encoding/json"
	"strings"
)

// PostgreSQL jsonb cannot store U+0000. Render it visibly without dropping
// command output. Decode first so literal backslash-u0000 text is unchanged.
func marshalEventData(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil || !bytes.Contains(encoded, []byte(`\u0000`)) {
		return encoded, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return json.Marshal(visibleNUL(decoded))
}

func visibleNUL(value any) any {
	switch value := value.(type) {
	case string:
		return strings.ReplaceAll(value, "\x00", `\0`)
	case []any:
		for i := range value {
			value[i] = visibleNUL(value[i])
		}
	case map[string]any:
		cleaned := make(map[string]any, len(value))
		for key, item := range value {
			cleaned[strings.ReplaceAll(key, "\x00", `\0`)] = visibleNUL(item)
		}
		return cleaned
	}
	return value
}
