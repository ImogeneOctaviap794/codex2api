package proxy

import "testing"

func TestIsMissingScopeUnauthorized(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "missing scope for responses write",
			body: `{"error":{"message":"Missing required scope: api.responses.write","type":"invalid_request_error","code":"missing_scope"}}`,
			want: true,
		},
		{
			name: "missing scope generic message",
			body: `{"error":{"message":"missing scope for this operation","type":"invalid_request_error","code":"missing_scope"}}`,
			want: true,
		},
		{
			name: "unauthorized invalid api key",
			body: `{"error":{"message":"Invalid API key","type":"invalid_request_error","code":"invalid_api_key"}}`,
			want: false,
		},
		{
			name: "empty body",
			body: ``,
			want: false,
		},
		{
			name: "invalid json",
			body: `not-json`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMissingScopeUnauthorized([]byte(tt.body))
			if got != tt.want {
				t.Fatalf("isMissingScopeUnauthorized() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsDeactivatedWorkspace(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "detail.code deactivated_workspace",
			body: `{"detail":{"code":"deactivated_workspace"}}`,
			want: true,
		},
		{
			name: "error.code deactivated_workspace",
			body: `{"error":{"code":"deactivated_workspace","message":"Workspace has been deactivated"}}`,
			want: true,
		},
		{
			name: "case insensitive match",
			body: `{"detail":{"code":"Deactivated_Workspace"}}`,
			want: true,
		},
		{
			name: "unrelated 402 shape",
			body: `{"error":{"code":"insufficient_quota","message":"You exceeded your quota"}}`,
			want: false,
		},
		{
			name: "401 missing_scope",
			body: `{"error":{"message":"Missing scope","code":"missing_scope"}}`,
			want: false,
		},
		{
			name: "empty body",
			body: ``,
			want: false,
		},
		{
			name: "invalid json",
			body: `not-json`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isDeactivatedWorkspace([]byte(tt.body))
			if got != tt.want {
				t.Fatalf("isDeactivatedWorkspace() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBodyRequiresPaidAccount(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "image_generation in tools",
			body: `{"model":"gpt-5.4-mini","tools":[{"type":"image_generation","size":"1024x1024"}]}`,
			want: true,
		},
		{
			name: "image_generation in tool_choice only",
			body: `{"model":"gpt-5.4","tool_choice":{"type":"image_generation"}}`,
			want: true,
		},
		{
			name: "mixed tools with image_generation",
			body: `{"tools":[{"type":"function","function":{"name":"x"}},{"type":"image_generation"}]}`,
			want: true,
		},
		{
			name: "plain function tools",
			body: `{"tools":[{"type":"function","function":{"name":"x"}}]}`,
			want: false,
		},
		{
			name: "no tools",
			body: `{"model":"gpt-5.4","messages":[]}`,
			want: false,
		},
		{
			name: "empty body",
			body: ``,
			want: false,
		},
		{
			name: "invalid json",
			body: `not-json`,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bodyRequiresPaidAccount([]byte(tt.body))
			if got != tt.want {
				t.Fatalf("bodyRequiresPaidAccount() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestModelOverrideInjectsImageGeneration(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{
			name:    "inject tools image_generation",
			payload: `{"gpt-draw":{"base_model":"gpt-5.4-mini","inject":{"tools":[{"type":"image_generation"}]}}}`,
			want:    true,
		},
		{
			name:    "inject tool_choice image_generation",
			payload: `{"gpt-draw":{"base_model":"gpt-5.4-mini","inject":{"tool_choice":{"type":"image_generation"}}}}`,
			want:    true,
		},
		{
			name:    "reasoning-only override",
			payload: `{"gpt-5.4-high":{"base_model":"gpt-5.4","inject":{"reasoning":{"effort":"high"}}}}`,
			want:    false,
		},
		{
			name:    "empty inject",
			payload: `{"gpt-alias":{"base_model":"gpt-5.4","inject":{}}}`,
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := ParseModelOverrides(tt.payload)
			for _, ov := range m {
				got := ov.InjectsImageGeneration()
				if got != tt.want {
					t.Fatalf("InjectsImageGeneration() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}
