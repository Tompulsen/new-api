package billing_setting

import (
	"fmt"
	"slices"

	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
)

const VideoBillingOption = "VideoBillingBindings"

func VideoUsageSchema() map[string]jsplugin.UsageFieldSchema {
	return map[string]jsplugin.UsageFieldSchema{
		"seconds": {
			Type: "number", Unit: "second",
			Description: jsplugin.LocalizedText{"en": "Video generation unit price", "zh": "视频生成单价"},
		},
		"resolution": {
			Enum:        []string{"480p", "720p", "1080p"},
			Description: jsplugin.LocalizedText{"en": "Output video resolution", "zh": "输出视频分辨率"},
		},
	}
}

// Missing resolution must not silently select the last branch of a price table.
func ValidateVideoExprFacts(expression string, facts map[string]any) error {
	if _, used := billingexpr.UsedUsageKeys(expression)["resolution"]; !used {
		return nil
	}
	resolution, _ := facts["resolution"].(string)
	if !slices.Contains([]string{"480p", "720p", "1080p"}, resolution) {
		return fmt.Errorf("video billing requires an explicit resolution: 480p, 720p or 1080p")
	}
	return nil
}
