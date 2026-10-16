package proxy_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestChatCompletionsPreservesToolSchemaNumbers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		parameters string
	}{
		{"plain", `{"type":"object","properties":{"id":{"type":"integer","enum":[9007199254740993]}}}`},
		{"referenced", `{"type":"object","$defs":{"ID":{"type":"integer","enum":[9007199254740993]}},"properties":{"id":{"$ref":"#/$defs/ID"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertUpstreamToolParameters(t, tc.parameters, `{"type":"object","properties":{"id":{"type":"integer","enum":[9007199254740993]}}}`)
		})
	}
}

func TestChatCompletionsPreservesToolSchemaData(t *testing.T) {
	for _, tc := range []struct{ name, parameters string }{
		{"property_names", `{"type":"object","properties":{"definitions":{"type":"object"},"$defs":{"type":"string"},"$ref":{"type":"string"}},"required":["definitions"]}`},
		{"literal_values", `{"type":"object","properties":{"value":{"type":"object","default":{"$ref":"#/$defs/missing"},"examples":[{"definitions":{"x":1}}],"const":{"$defs":{"x":2}}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertUpstreamToolParameters(t, tc.parameters, tc.parameters)
		})
	}
}

func TestChatCompletionsExpandsNestedToolSchemas(t *testing.T) {
	assertUpstreamToolParameters(t,
		`{"type":"object","$defs":{"Text":{"type":"string"}},"properties":{"items":{"type":"array","items":{"anyOf":[{"$ref":"#/$defs/Text"},{"type":"null"}]}}},"additionalProperties":{"$ref":"#/$defs/Text"}}`,
		`{"type":"object","properties":{"items":{"type":"array","items":{"anyOf":[{"type":"string"},{"type":"null"}]}}},"additionalProperties":{"type":"string"}}`)
}

func assertUpstreamToolParameters(t *testing.T, parameters, wantParameters string) {
	t.Helper()
	observations := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			return
		}
		observations <- body
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	server := newProxyServer(t, []providerFixture{
		{baseURL: upstream.URL + "/v1", apiKey: "provider-secret", modelAlias: "provider-model", priority: 1},
	})
	defer server.Close()
	requestBody := `{"model":"virtual","messages":[],"tools":[{"type":"function","function":{"name":"demo","parameters":` + parameters + `}}]}`
	response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.StatusCode, responseBody)
	}
	select {
	case body := <-observations:
		var payload struct {
			Tools []struct {
				Function struct {
					Parameters json.RawMessage `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || len(payload.Tools) != 1 {
			t.Fatalf("unexpected upstream tools: %s; error = %v", body, err)
		}
		decode := func(raw []byte) any {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			var value any
			if err := decoder.Decode(&value); err != nil {
				t.Fatalf("decode parameters: %v", err)
			}
			return value
		}
		got := payload.Tools[0].Function.Parameters
		if !reflect.DeepEqual(decode(got), decode([]byte(wantParameters))) {
			t.Fatalf("upstream parameters = %s, want %s", got, wantParameters)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream request not observed")
	}
}
