package server

import (
	"testing"

	"github.com/Kiowx/opencode-cc/internal/config"
	"github.com/Kiowx/opencode-cc/internal/proxy"
)

func TestApplyThinkingBudgetMappingNormalizesAdaptiveForGLM(t *testing.T) {
	tests := []struct {
		name       string
		thinking   *proxy.AnthropicThinking
		wantType   string
		wantBudget int
	}{
		{name: "default", wantType: "enabled"},
		{name: "adaptive", thinking: &proxy.AnthropicThinking{Type: "adaptive", BudgetTokens: 1024}, wantType: "auto"},
		{name: "adaptive case insensitive", thinking: &proxy.AnthropicThinking{Type: "Adaptive"}, wantType: "auto"},
		{name: "enabled", thinking: &proxy.AnthropicThinking{Type: "enabled", BudgetTokens: 4096}, wantType: "enabled", wantBudget: 4096},
		{name: "disabled", thinking: &proxy.AnthropicThinking{Type: "disabled", BudgetTokens: 4096}, wantType: "disabled"},
		{name: "auto", thinking: &proxy.AnthropicThinking{Type: "auto", BudgetTokens: 4096}, wantType: "auto"},
		{name: "custom value preserved", thinking: &proxy.AnthropicThinking{Type: "provider-specific"}, wantType: "provider-specific"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &proxy.OpenAIRequest{}
			areq := &proxy.AnthropicRequest{Thinking: tt.thinking}
			applyThinkingBudgetMapping(req, areq, "glm-5.2", config.Default())

			if req.Thinking == nil || req.Thinking.Type != tt.wantType {
				t.Fatalf("thinking = %+v, want type %q", req.Thinking, tt.wantType)
			}
			if tt.wantBudget == 0 {
				if req.Thinking.BudgetTokens != nil {
					t.Fatalf("budget_tokens = %d, want omitted", *req.Thinking.BudgetTokens)
				}
			} else if req.Thinking.BudgetTokens == nil || *req.Thinking.BudgetTokens != tt.wantBudget {
				t.Fatalf("budget_tokens = %v, want %d", req.Thinking.BudgetTokens, tt.wantBudget)
			}
		})
	}
}
