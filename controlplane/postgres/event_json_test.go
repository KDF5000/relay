package postgres

import (
	"encoding/json"
	"testing"
)

func TestMarshalEventDataNUL(t *testing.T) {
	input := json.RawMessage(`{"delta":"a\u0000中文\n","literal":"\\u0000","nested":["\u0000"],"number":9007199254740993}`)
	encoded, err := marshalEventData(input)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"delta":"a\\0中文\n","literal":"\\u0000","nested":["\\0"],"number":9007199254740993}`
	if string(encoded) != want {
		t.Fatalf("got %s, want %s", encoded, want)
	}
}
