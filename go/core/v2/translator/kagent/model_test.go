package kagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	v2translator "github.com/kagent-dev/kagent/go/core/v2/translator"
)

func TestTranslateOpenAIServiceTierFailsClosed(t *testing.T) {
	format := v1alpha3.OpenAIAPIFormatResponses
	tier := v1alpha3.OpenAIServiceTierFast
	model, data, err := NewCompiler(nil).translateModel(context.Background(), &v1alpha3.ModelConfig{
		Spec: v1alpha3.ModelConfigSpec{
			Provider: v1alpha3.ModelProviderOpenAI,
			Model:    "gpt-test",
			OpenAI: &v1alpha3.OpenAIConfig{
				APIFormat:   &format,
				ServiceTier: &tier,
			},
		},
	})
	if model != nil || data != nil {
		t.Fatalf("translateModel() = %T, %#v, want nil results", model, data)
	}
	var validation *v2translator.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("translateModel() error = %v, want validation error", err)
	}
	if !strings.Contains(err.Error(), "supported only by the Codex Harness") {
		t.Fatalf("translateModel() error = %q", err)
	}
}
