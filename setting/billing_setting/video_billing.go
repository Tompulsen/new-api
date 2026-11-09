package billing_setting

import (
	"fmt"
	"maps"
	"math"
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

// An unknown output resolution reserves the most expensive configured tier,
// without persisting a guessed resolution as an actual usage fact.
func EstimateVideoExpr(expression string, facts map[string]any) (float64, billingexpr.TraceResult, error) {
	resolutions := []string{""}
	if _, used := billingexpr.UsedUsageKeys(expression)["resolution"]; used {
		resolution, _ := facts["resolution"].(string)
		if resolution == "" || resolution == "auto" {
			resolutions = VideoUsageSchema()["resolution"].Enum
		} else if err := ValidateVideoExprFacts(expression, facts); err != nil {
			return 0, billingexpr.TraceResult{}, err
		}
	}
	var maximum float64
	var selected billingexpr.TraceResult
	for i, resolution := range resolutions {
		candidate := make(map[string]any, len(facts)+1)
		maps.Copy(candidate, facts)
		if resolution != "" {
			candidate["resolution"] = resolution
		}
		cost, trace, err := billingexpr.RunExprWithRequest(expression, billingexpr.TokenParams{}, billingexpr.RequestInput{Usage: candidate})
		if err != nil {
			return 0, trace, err
		}
		if cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
			return 0, trace, fmt.Errorf("video expression produced an invalid reservation")
		}
		if i == 0 || cost > maximum {
			maximum, selected = cost, trace
		}
	}
	return maximum, selected, nil
}
