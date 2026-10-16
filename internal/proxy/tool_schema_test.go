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

func TestChatCompletionsResolvesToolSchemaPointers(t *testing.T) {
	for _, tc := range []struct{ name, parameters, want string }{
		{"namespaces", `{"type":"object","$defs":{"X":{"type":"string"}},"definitions":{"X":{"type":"integer"}},"properties":{"new":{"$ref":"#/$defs/X"},"old":{"$ref":"#/definitions/X"}}}`, `{"type":"object","properties":{"new":{"type":"string"},"old":{"type":"integer"}}}`},
		{"escaped_tokens", `{"type":"object","$defs":{"a/b~c":{"type":"string"}},"properties":{"value":{"$ref":"#/$defs/a~1b~0c"}}}`, `{"type":"object","properties":{"value":{"type":"string"}}}`},
		{"percent_encoding", `{"type":"object","$defs":{"a b":{"type":"string"}},"properties":{"value":{"$ref":"#/$defs/a%20b"}}}`, `{"type":"object","properties":{"value":{"type":"string"}}}`},
		{"nested_pointer", `{"type":"object","$defs":{"Item":{"properties":{"name":{"type":"string"}}}},"properties":{"value":{"$ref":"#/$defs/Item/properties/name"}}}`, `{"type":"object","properties":{"value":{"type":"string"}}}`},
		{"array_pointer", `{"type":"object","$defs":{"Item":{"anyOf":[{"type":"string"}]}},"properties":{"value":{"$ref":"#/$defs/Item/anyOf/0"}}}`, `{"type":"object","properties":{"value":{"type":"string"}}}`},
		{"root_scope", `{"type":"object","$defs":{"X":{"type":"string"}},"properties":{"value":{"$defs":{"X":{"type":"integer"}},"$ref":"#/$defs/X"}}}`, `{"type":"object","properties":{"value":{"type":"string"}}}`},
		{"reused_chain", `{"type":"object","$defs":{"X":{"$ref":"#/$defs/Y"},"Y":{"type":"string"}},"properties":{"a":{"$ref":"#/$defs/X"},"b":{"$ref":"#/$defs/X"}}}`, `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertUpstreamToolParameters(t, tc.parameters, tc.want)
		})
	}
}

func TestChatCompletionsPreservesToolReferenceConstraints(t *testing.T) {
	for _, tc := range []struct{ name, parameters, want string }{
		{"conflicting_limits", `{"type":"object","$defs":{"Text":{"type":"string","maxLength":10}},"properties":{"value":{"$ref":"#/$defs/Text","maxLength":1}}}`, `{"type":"object","properties":{"value":{"maxLength":1,"allOf":[{"type":"string","maxLength":10}]}}}`},
		{"existing_allOf", `{"type":"object","$defs":{"Text":{"type":"string"}},"properties":{"value":{"$ref":"#/$defs/Text","allOf":[{"minLength":1}]}}}`, `{"type":"object","properties":{"value":{"allOf":[{"minLength":1},{"type":"string"}]}}}`},
		{"separate_property_scopes", `{"type":"object","$defs":{"Closed":{"type":"object","properties":{"x":{"type":"string"}},"additionalProperties":false}},"properties":{"value":{"$ref":"#/$defs/Closed","properties":{"y":{"type":"string"}}}}}`, `{"type":"object","properties":{"value":{"properties":{"y":{"type":"string"}},"allOf":[{"type":"object","properties":{"x":{"type":"string"}},"additionalProperties":false}]}}}`},
		{"boolean_definition", `{"type":"object","$defs":{"Allowed":true},"properties":{"value":{"$ref":"#/$defs/Allowed"}}}`, `{"type":"object","properties":{"value":true}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertUpstreamToolParameters(t, tc.parameters, tc.want)
		})
	}
}

func TestChatCompletionsReportsInvalidToolReferences(t *testing.T) {
	for _, tc := range []struct{ name, parameters, diagnosis, ref string }{
		{"unresolved", `{"properties":{"value":{"$ref":"#/$defs/Missing"}}}`, "unresolved", "#/$defs/Missing"},
		{"cyclic", `{"$defs":{"Node":{"$ref":"#/$defs/Node"}},"properties":{"value":{"$ref":"#/$defs/Node"}}}`, "cyclic", "#/$defs/Node"},
		{"indirect_cycle", `{"$defs":{"A":{"$ref":"#/definitions/B"}},"definitions":{"B":{"$ref":"#/$defs/A"}},"properties":{"value":{"$ref":"#/$defs/A"}}}`, "cyclic", "#/$defs/A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("invalid schema reached upstream")
			}))
			defer upstream.Close()
			server := newProxyServer(t, []providerFixture{
				{baseURL: upstream.URL + "/v1", apiKey: "provider-secret", modelAlias: "provider-model", priority: 1},
			})
			defer server.Close()
			requestBody := `{"messages":[],"tools":[{"type":"function","function":{"name":"demo","parameters":` + tc.parameters + `}}]}`
			response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(requestBody))
			if err != nil {
				t.Fatalf("proxy request: %v", err)
			}
			defer response.Body.Close()
			var envelope struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if response.StatusCode != http.StatusBadGateway || envelope.Error.Type != "server_error" || envelope.Error.Code != "upstream_encode_error" {
				t.Fatalf("unexpected error response: status=%d envelope=%+v", response.StatusCode, envelope)
			}
			for _, want := range []string{"tool 0 parameters", tc.diagnosis, tc.ref} {
				if !strings.Contains(envelope.Error.Message, want) {
					t.Errorf("message = %q, want %q", envelope.Error.Message, want)
				}
			}
		})
	}
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
